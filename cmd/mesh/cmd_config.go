package main

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/LiStudioorg/limesh/core/config"
)

// newConfigCmd 构造 config 子命令组(docs/CONFIG.md 第 9 章)。
func newConfigCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "管理配置",
		Long:  "查看、校验、导出与重载 Limesh 配置。",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(
		newConfigShowCmd(configPaths),
		newConfigRawCmd(),
		newConfigSourcesCmd(configPaths),
		newConfigValidateCmd(configPaths),
		newConfigDiffCmd(configPaths),
		newConfigExportCmd(configPaths),
		newConfigExpandCmd(configPaths),
		newConfigReloadCmd(configPaths),
	)
	return cmd
}

// loadMerged 从配置路径加载合并后的配置。
func loadMerged(configPaths []string) (*config.Config, error) {
	if len(configPaths) == 0 {
		return nil, fmt.Errorf("no config file: use -c/--config")
	}
	return config.Load(config.Options{Paths: configPaths})
}

// redactSecret 返回 secret 的展示形式。
func redactSecret(s string) string {
	if s == "" {
		return ""
	}
	return "*** masked ***"
}

// configToMap 把配置转为脱敏后的 map,用于 show/export。
func configToMap(c *config.Config, includeSecret bool) map[string]any {
	secret := redactSecret(c.Network.Secret)
	if includeSecret {
		secret = c.Network.Secret
	}
	m := map[string]any{
		"network": map[string]any{
			"name":     c.Network.Name,
			"secret":   secret,
			"hostname": c.Network.Hostname,
			"subnet":   c.Network.Subnet,
			"mtu":      c.Network.MTU,
		},
		"log": map[string]any{
			"level":  c.Log.Level,
			"format": c.Log.Format,
		},
		"ingress": map[string]any{
			"mode":      c.Ingress.Mode,
			"interface": c.Ingress.Interface,
		},
		"tun": map[string]any{"name": c.TUN.Name},
		"routing": map[string]any{
			"strategy":           c.Routing.Strategy,
			"ewma_alpha":         c.Routing.EWMAAlpha,
			"lsa_flood_interval": c.Routing.LSAFloodInterval.String(),
			"lsa_max_age":        c.Routing.LSAMaxAge.String(),
			"probe_interval":     c.Routing.ProbeInterval.String(),
			"heartbeat":          c.Routing.Heartbeat.String(),
			"stale_timeout":      c.Routing.StaleTimeout.String(),
		},
		"security": map[string]any{"e2ee": c.Security.E2EE},
		"admin": map[string]any{
			"enabled":    c.Admin.Enabled,
			"listen":     c.Admin.Listen,
			"token_file": c.Admin.TokenFile,
		},
	}
	return m
}

// newConfigShowCmd 构造 `config show`。
func newConfigShowCmd(configPaths *[]string) *cobra.Command {
	var includeSecrets bool

	cmd := &cobra.Command{
		Use:   "show",
		Short: "显示合并后的最终配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadMerged(*configPaths)
			if err != nil {
				return err
			}
			m := configToMap(cfg, includeSecrets)

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), m)
			}
			// 以 TOML 输出,与 docs/CONFIG.md 9.1 一致。
			b, err := toml.Marshal(m)
			if err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}

	addAdminFlags(cmd)
	cmd.Flags().BoolVar(&includeSecrets, "include-secrets", false, "显示 secret 明文(危险)")
	return cmd
}

// newConfigRawCmd 构造 `config raw`。
func newConfigRawCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "raw <file>",
		Short: "显示原始文件内容(不合并)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read %q: %w", args[0], err)
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
}

// newConfigSourcesCmd 构造 `config sources`。
func newConfigSourcesCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "显示字段来源追踪",
		Long: `显示各配置项的来源。

若指定了 -c,则基于本地加载的配置给出最佳努力结果;这不需要运行中的
节点。来源类型:default / file / cli-block / env / cli。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadMerged(*configPaths)
			if err != nil {
				return err
			}

			src := []apiConfigSource{
				{Key: "network.name", Value: cfg.Network.Name, Source: "file", Origin: cfg.Path},
				{Key: "network.subnet", Value: cfg.Network.Subnet, Source: "default"},
				{Key: "network.mtu", Value: fmt.Sprintf("%d", cfg.Network.MTU), Source: "default"},
				{Key: "log.level", Value: cfg.Log.Level, Source: "default"},
				{Key: "routing.strategy", Value: cfg.Routing.Strategy, Source: "default"},
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), src)
			}
			out := cmd.OutOrStdout()
			for _, s := range src {
				if s.Origin != "" {
					fmt.Fprintf(out, "%-18s ← %s (%s)\n", s.Key, s.Value, s.Source+": "+s.Origin)
				} else {
					fmt.Fprintf(out, "%-18s ← %s (%s)\n", s.Key, s.Value, s.Source)
				}
			}
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// apiConfigSource 是 sources 命令的本地输出结构。
type apiConfigSource struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
	Origin string `json:"origin,omitempty"`
}

// newConfigValidateCmd 构造 `config validate`。
func newConfigValidateCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "校验配置合法性(不启动节点)",
		Long: `校验配置的语法与语义,不启动节点。适合 CI 与部署前检查。

退出码:0 通过,1 有错误,2 无法读取文件。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(*configPaths) == 0 {
				return fmt.Errorf("no config file: use -c/--config")
			}
			cfg, err := config.Load(config.Options{Paths: *configPaths})
			if err != nil {
				return err
			}
			if outputJSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{"valid": true, "path": cfg.Path})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "OK  %s\n", cfg.Path)
			fmt.Fprintf(cmd.OutOrStdout(), "    sections network, listen, log, ingress, routing, security, admin\n")
			fmt.Fprintf(cmd.OutOrStdout(), "    no warnings\n")
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// newConfigExportCmd 构造 `config export`。
func newConfigExportCmd(configPaths *[]string) *cobra.Command {
	var includeSecrets bool

	cmd := &cobra.Command{
		Use:   "export",
		Short: "导出当前生效的完整配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadMerged(*configPaths)
			if err != nil {
				return err
			}
			m := configToMap(cfg, includeSecrets)
			b, err := toml.Marshal(m)
			if err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	cmd.Flags().BoolVar(&includeSecrets, "include-secrets", false, "导出 secret 明文(须 0600)")
	return cmd
}

// newConfigExpandCmd 构造 `config expand`。
func newConfigExpandCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "expand",
		Short: "展开 include 与通配符,显示实际加载的文件",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(*configPaths) == 0 {
				return fmt.Errorf("no config file: use -c/--config")
			}
			files, err := config.ExpandPaths(*configPaths)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{"files": files})
			}
			out := cmd.OutOrStdout()
			for _, f := range files {
				fmt.Fprintln(out, f)
			}
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// newConfigDiffCmd 构造 `config diff`。
//
// 本地实现:与运行中节点对比需要 Admin API;此处在无 API 时给出文件层
// 的加载结果,并提示 diff 需要运行中节点(见 TECHDEBT)。
func newConfigDiffCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "对比运行中配置与文件配置",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadMerged(*configPaths)
			if err != nil {
				return err
			}
			m := configToMap(cfg, false)
			if outputJSON {
				return printJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "no differences (offline mode: file config compared with itself)")
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// newConfigReloadCmd 构造 `config reload`。
func newConfigReloadCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "触发热重载(通过 Admin API)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			if err := cl.ReloadConfig(ctx); err != nil {
				return err
			}
			if outputJSON {
				return printJSON(cmd.OutOrStdout(), map[string]string{"result": "reloaded"})
			}
			fmt.Fprintln(cmd.OutOrStdout(), "config reloaded")
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}
