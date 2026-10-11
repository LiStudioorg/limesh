package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newStatusCmd 构造 status 子命令。
func newStatusCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "显示运行中节点的状态",
		Long: `查询运行中节点的整体状态:身份、虚拟 IP、运行时长、对端与路由数量。

需要节点已启用 Admin API(配置 [admin].enabled = true)。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			st, err := cl.Status(ctx)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), st)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Node ID:    %s\n", st.NodeID)
			fmt.Fprintf(out, "Hostname:   %s\n", st.Hostname)
			fmt.Fprintf(out, "Virtual IP: %s\n", st.VirtualIP)
			fmt.Fprintf(out, "Uptime:     %s\n", formatDuration(st.UptimeSeconds))
			fmt.Fprintf(out, "Peers:      %d (%d direct, %d relayed)\n",
				st.PeerCount, st.DirectPeers, st.RelayedPeers)
			fmt.Fprintf(out, "Routes:     %d\n", st.RouteCount)
			if st.IngressMode != "" {
				fmt.Fprintf(out, "Ingress:    %s\n", st.IngressMode)
			}
			fmt.Fprintf(out, "Version:    %s\n", st.Version)
			return nil
		},
	}

	addAdminFlags(cmd)
	return cmd
}
