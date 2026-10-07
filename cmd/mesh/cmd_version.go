package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// newVersionCmd 构造 version 子命令。
func newVersionCmd() *cobra.Command {
	var short bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		Long:  "显示 mesh 的版本、提交哈希与构建时间。",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			if short {
				fmt.Fprintln(out, version)
				return nil
			}

			fmt.Fprintf(out, "mesh %s (commit: %s, built: %s)\n", version, commit, buildTime)
			if verbose {
				fmt.Fprintf(out, "go:       %s\n", runtime.Version())
				fmt.Fprintf(out, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&short, "short", false, "只输出版本号")
	return cmd
}

// verbose 由 node 子命令的 -v 共享,用于 version 输出更多细节。
var verbose bool
