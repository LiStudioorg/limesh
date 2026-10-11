package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newNATCmd 构造 nat 子命令组。
func newNATCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nat",
		Short: "查看 NAT 类型与候选地址",
		Long: `查看运行中节点观测到的 NAT 类型与公网映射地址,或列出候选地址。

数据来自节点启动时的 STUN 探测与网卡枚举,并周期性刷新。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNATShow(cmd, *configPaths)
		},
	}

	addAdminFlags(cmd)
	cmd.AddCommand(newNATCandidatesCmd(configPaths))
	cmd.AddCommand(newNATProbeCmd(configPaths))
	return cmd
}

// runNATShow 输出 NAT 概览。
func runNATShow(cmd *cobra.Command, configPaths []string) error {
	cl, err := newAdminClient(cmd, configPaths)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()

	info, err := cl.NAT(ctx)
	if err != nil {
		return err
	}

	if outputJSON {
		return printJSON(cmd.OutOrStdout(), info)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "NAT Type:     %s\n", natTypeLabel(info.Type))
	if info.Mapped != "" {
		fmt.Fprintf(out, "Mapped Addr:  %s\n", info.Mapped)
	} else {
		fmt.Fprintf(out, "Mapped Addr:  (unknown)\n")
	}
	if !info.DetectedAt.IsZero() {
		fmt.Fprintf(out, "Detected:     %s\n", info.DetectedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(out, "Candidates:   %d\n", info.CandidateCount)
	return nil
}

// newNATCandidatesCmd 构造 `nat candidates`。
func newNATCandidatesCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "candidates",
		Short: "列出候选地址",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			cands, err := cl.Candidates(ctx)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), cands)
			}

			rows := make([][]string, 0, len(cands))
			for _, c := range cands {
				rows = append(rows, []string{
					c.Type,
					c.Addr,
					fmt.Sprintf("%d", c.Priority),
					c.Source,
				})
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no candidates")
				return nil
			}
			writeTable(cmd.OutOrStdout(),
				[]string{"TYPE", "ADDRESS", "PRIORITY", "SOURCE"}, rows)
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// newNATProbeCmd 构造 `nat probe`。
//
// 注意:探测在**节点进程内**同步执行,CLI 只触发并通过 API 读取结果。
// 当前 API 未提供"强制重探"端点,因此该命令通过读取现有观测结果实现,
// 并在无结果时提示用户等待下一次周期刷新。
func newNATProbeCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "重新读取 NAT 探测结果",
		Long: `读取节点最近一次 NAT 探测结果。

说明:实际探测由节点进程周期执行;本命令触发一次结果读取。
强制立即重探(不需等待周期)属后续阶段,届时会增加 API 写端点。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNATShow(cmd, *configPaths)
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// natTypeLabel 把 NAT 类型转为带中文说明的标签。
func natTypeLabel(t string) string {
	switch t {
	case "open":
		return "open (公网,无 NAT)"
	case "cone":
		return "cone (锥形)"
	case "symmetric":
		return "symmetric (对称)"
	default:
		return "unknown (未知)"
	}
}
