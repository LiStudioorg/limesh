package main

import (
	"github.com/spf13/cobra"
)

// newRootCmd 构造 cobra 根命令。
//
// 未指定子命令时默认执行 node —— 使 `mesh -c config.toml` 这类最常用写法
// 无需显式写出子命令。
func newRootCmd() *cobra.Command {
	var configPaths []string

	root := &cobra.Command{
		Use:   "mesh",
		Short: "Limesh —— 轻量级去中心化组网平台",
		Long: `Limesh(立网)把分散在家庭、云、机房、容器里的机器虚拟成一个扁平的三层网络。

每台机器获得一个稳定的虚拟 IP,彼此可以直接发 IP 包,不关心底层是
NAT、防火墙还是跨洲链路。单个二进制承载全部角色。

  Lightweight mesh. Instantly.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// 无子命令时执行 node。
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNode(cmd, configPaths)
		},
	}

	// 全局 flag:-c/--config 可重复,支持通配符。
	root.PersistentFlags().StringArrayVarP(&configPaths, "config", "c", nil,
		"配置文件路径,可重复;支持 * 与 ** 通配符")

	// -v 是全局概念(提升日志详细度),在根命令注册使所有子命令共享,
	// 避免每个子命令各自声明导致 `version -v` 这类调用失败。
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false,
		"输出详细日志")

	root.AddCommand(newNodeCmd(&configPaths))
	root.AddCommand(newVersionCmd())

	return root
}
