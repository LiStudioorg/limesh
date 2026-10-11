package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newPeerCmd 构造 peer 子命令组。
func newPeerCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer",
		Short: "管理对端",
		Long:  "查询运行中节点的对端信息。",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(newPeerListCmd(configPaths))
	cmd.AddCommand(newPeerShowCmd(configPaths))
	return cmd
}

// newPeerListCmd 构造 `peer list`。
func newPeerListCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出全部对端",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			peers, err := cl.Peers(ctx)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), peers)
			}

			rows := make([][]string, 0, len(peers))
			for _, p := range peers {
				rows = append(rows, []string{
					shortID(p.NodeID),
					p.VirtualIP,
					p.State,
					formatRTT(p.RTTMillis),
					fmt.Sprintf("%.1f%%", p.LossRate*100),
					p.Addr,
				})
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no peers")
				return nil
			}
			writeTable(cmd.OutOrStdout(),
				[]string{"NODE ID", "VIRTUAL IP", "STATE", "RTT", "LOSS", "ADDR"},
				rows)
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}

// newPeerShowCmd 构造 `peer show <nodeID>`。
func newPeerShowCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <nodeID>",
		Short: "显示单个对端的详细信息",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			p, err := cl.Peer(ctx, args[0])
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), p)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Node ID:    %s\n", p.NodeID)
			fmt.Fprintf(out, "Virtual IP: %s\n", p.VirtualIP)
			fmt.Fprintf(out, "State:      %s\n", p.State)
			fmt.Fprintf(out, "Address:    %s\n", p.Addr)
			fmt.Fprintf(out, "RTT:        %s\n", formatRTT(p.RTTMillis))
			fmt.Fprintf(out, "Loss:       %.1f%%\n", p.LossRate*100)
			fmt.Fprintf(out, "Session:    %d\n", p.SessionID)
			fmt.Fprintf(out, "Last Seen:  %s\n", p.LastSeen.Format("2006-01-02 15:04:05"))
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}
