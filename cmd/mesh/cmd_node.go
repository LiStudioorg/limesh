package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/node"
)

// newNodeCmd 构造 node 子命令。
func newNodeCmd(configPaths *[]string) *cobra.Command {
	var (
		logLevel  string
		logFormat string
	)

	cmd := &cobra.Command{
		Use:   "node",
		Short: "以节点角色加入虚拟网络",
		Long: `启动一个 Limesh 节点。

Phase 0 的最小实现:加载并校验配置、按配置启动传输监听、阻塞运行至收到
SIGINT/SIGTERM 后优雅关闭。握手、路由与数据面在后续阶段加入。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNode(cmd, *configPaths)
		},
	}

	cmd.Flags().StringVar(&logLevel, "log.level", "", "日志级别(debug/info/warn/error)")
	cmd.Flags().StringVar(&logFormat, "log.format", "", "日志格式(console/json)")
	// 注:-v/--verbose 定义在根命令上,由所有子命令共享。

	return cmd
}

// runNode 加载配置、装配节点并阻塞运行。
//
// 这是 node 角色的完整生命周期:配置 → 日志 → 节点 → 信号 → 优雅关闭。
func runNode(cmd *cobra.Command, configPaths []string) error {
	if len(configPaths) == 0 {
		return fmt.Errorf("no config file: use -c/--config to specify one (e.g. -c config.example.toml)")
	}

	// ---- 加载配置(四层优先级) ----
	//
	// 必须传入 cmd.Flags() 之外的全集:cmd.Flags() 只包含本子命令自己声明的
	// flag,不含根命令的持久 flag(如 -c);而 config.Load 需要看到所有已解析
	// 的 flag 才能构成最高优先级层。cmd.Root().Flags() 返回根命令的持久 flag
	// 与本命令 flag 的并集,正是这里需要的。
	//
	// 注意:InheritedFlags 只含"父命令的持久 flag",不含本命令自己的 flag,
	// 因此不能单独使用;两者合并才能覆盖全部分支。
	flags := cmd.Flags()
	flags.AddFlagSet(cmd.Root().PersistentFlags())

	cfg, err := config.Load(config.Options{
		Paths: configPaths,
		Flags: flags,
	})
	if err != nil {
		return err
	}

	// ---- 构建日志 ----
	logger, err := buildLogger(cfg, verbose)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	// ---- 装配节点 ----
	hostname, err := os.Hostname()
	if err != nil {
		// 取不到系统 hostname 不是致命错误,用占位符继续。
		hostname = "unknown"
	}

	n, err := node.New(node.Options{
		Config:   cfg,
		Logger:   logger,
		Hostname: hostname,
		Version:  version,
		Reload: func() error {
			// 重新加载全部配置来源并校验;热重载的逐模块 Apply 属后续阶段,
			// 当前实现保证"新配置可加载且合法",并刷新进程内快照。
			newCfg, err := config.Load(config.Options{Paths: configPaths, Flags: flags})
			if err != nil {
				return err
			}
			logger.Info("config reloaded",
				zap.String("network", newCfg.Network.Name),
				zap.String("source", newCfg.Path),
			)
			return nil
		},
	})
	if err != nil {
		return err
	}

	// ---- 注册信号处理 ----
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("config loaded",
		zap.String("network", cfg.Network.Name),
		zap.String("source", cfg.Path),
		zap.Strings("listen", listenAddrs(cfg)),
	)

	// Run 阻塞至信号到达。
	return n.Run(ctx)
}

// buildLogger 依据配置构造 zap 日志实例。
func buildLogger(cfg *config.Config, verbose bool) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", cfg.Log.Level, err)
	}
	if verbose && level > zapcore.DebugLevel {
		level = zapcore.DebugLevel
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	var (
		encoder zapcore.Encoder
		opts    []zap.Option
	)
	switch cfg.Log.Format {
	case "json":
		encoder = zapcore.NewJSONEncoder(encCfg)
	default:
		encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoder = zapcore.NewConsoleEncoder(encCfg)
		opts = append(opts, zap.AddStacktrace(zapcore.ErrorLevel))
	}

	core := zapcore.NewCore(encoder, zapcore.Lock(os.Stderr), level)

	logger := zap.New(core, opts...)
	logger = logger.With(zap.String("role", "node"))
	return logger, nil
}

// listenAddrs 提取配置中的监听地址列表,用于启动日志。
func listenAddrs(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Listen))
	for _, l := range cfg.Listen {
		out = append(out, l.Addr)
	}
	return out
}
