package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/LiStudioorg/limesh/core/api"
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
	return printNATSnapshot(cmd, info)
}

// printNATSnapshot 以人类可读格式输出 NAT 观测快照。
func printNATSnapshot(cmd *cobra.Command, info *api.NATSnapshot) error {
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
// 与 `mesh nat` 的区别:`mesh nat` 读缓存,不触发探测;本命令通过
// `POST /api/v1/nat/probe` 强制节点同步重探并等待结果。
func newNATProbeCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "强制重新探测 NAT(同步等待)",
		Long: `强制节点立即重新执行一次 NAT 探测并同步等待结果。

服务端会阻塞直到探测完成(最长 10 秒);若已有探测在进行,返回 409。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			out := cmd.OutOrStdout()
			if !outputJSON {
				fmt.Fprintln(out, "Probing NAT type...")
			}

			info, err := cl.ProbeNAT(ctx)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(out, info)
			}
			return printNATSnapshot(cmd, info)
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
