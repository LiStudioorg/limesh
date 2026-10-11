package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/limesh/core/api"
	"github.com/LiStudioorg/limesh/core/config"
)

// 通用 Admin 相关 flag 变量。
//
// 这些 flag 注册在各查询子命令上,而非根命令:它们只对"与运行中节点通信"
// 的命令有意义,挂在根命令上会让 `mesh version --socket` 这类无意义调用
// 也通过解析。
var (
	// adminSocket 覆盖默认 Admin Socket 路径。
	adminSocket string
	// adminToken 覆盖 TCP 模式下的认证 token。
	adminToken string
	// outputJSON 使命令输出 JSON 而非表格/文本。
	outputJSON bool
	// noColor 禁用颜色输出。
	noColor bool
)

// addAdminFlags 给命令注册通用 Admin 查询 flag。
func addAdminFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&adminSocket, "socket", "", "覆盖默认 Admin Socket 路径")
	cmd.Flags().StringVar(&adminToken, "token", "", "TCP 模式下的认证 token")
	cmd.Flags().BoolVar(&outputJSON, "json", false, "以 JSON 输出")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "禁用颜色")
}

// newAdminClient 依据全局 -c 与 --socket/--token 构造 Admin 客户端。
//
// 优先级:--socket/--token > 配置文件的 [admin] 段。
// 配置文件从全局 -c 加载;无 -c 时用默认端点。
func newAdminClient(cmd *cobra.Command, configPaths []string) (*api.Client, error) {
	// --socket 允许写裸路径(如 /run/mesh/admin.sock)或带 scheme 的完整地址;
	// 裸路径按 Unix Socket 处理,方便日常使用。
	listen := adminSocket
	if listen != "" && !strings.Contains(listen, "://") {
		listen = "unix://" + listen
	}

	opts := api.ClientOptions{
		Listen:  listen,
		Token:   adminToken,
		Timeout: 10 * time.Second,
	}

	if len(configPaths) > 0 {
		cfg, err := config.Load(config.Options{Paths: configPaths})
		if err != nil {
			return nil, err
		}
		opts.Config = cfg
	} else {
		// 无配置文件:构造一个仅含默认端点的配置。
		cfg := config.Default()
		opts.Config = cfg
	}

	return api.NewClient(opts)
}

// withTimeout 返回带超时的 context 及其 cancel。
func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// printJSON 以缩进 JSON 输出。
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTable 打印一个简单对齐的表格。
//
// 列宽按内容自适应;空单元格以 "-" 占位。
func writeTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	printRow := func(cells []string) {
		var b strings.Builder
		for i := range headers {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteString(cell)
			if i < len(headers)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)+2))
			}
		}
		fmt.Fprintln(w, b.String())
	}

	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

// shortID 截断 NodeID 便于表格展示。
func shortID(id string) string {
	const n = 12
	if len(id) <= n {
		return id
	}
	return id[:n]
}

// formatRTT 把毫秒数格式化为人类可读形式。
func formatRTT(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	if ms < 1 {
		return fmt.Sprintf("%.2fms", ms)
	}
	if ms < 1000 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}

// formatDuration 把秒数格式化为紧凑时长。
func formatDuration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
