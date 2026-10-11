package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newRouteCmd 构造 route 子命令组。
func newRouteCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "管理路由",
		Long:  "查询运行中节点的路由表。",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newRouteListCmd(configPaths))
	return cmd
}

// newRouteListCmd 构造 `route list`。
func newRouteListCmd(configPaths *[]string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出路由表",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newAdminClient(cmd, *configPaths)
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()

			routes, err := cl.Routes(ctx)
			if err != nil {
				return err
			}

			if outputJSON {
				return printJSON(cmd.OutOrStdout(), routes)
			}

			rows := make([][]string, 0, len(routes))
			for _, r := range routes {
				rows = append(rows, []string{
					r.DstVirtualIP,
					shortID(r.NextHop),
					fmt.Sprintf("%d", r.Cost),
					fmt.Sprintf("%d", r.Hops),
				})
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no routes")
				return nil
			}
			writeTable(cmd.OutOrStdout(),
				[]string{"DESTINATION", "NEXT HOP", "COST", "HOPS"}, rows)
			return nil
		},
	}
	addAdminFlags(cmd)
	return cmd
}
