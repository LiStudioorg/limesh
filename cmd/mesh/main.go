// Command mesh 是 Limesh 的唯一可执行入口。
//
// 单二进制承载全部角色:客户端、中继、控制面、网关、出口节点都编译进同一个
// 可执行文件,由子命令在运行时选择(见 AGENTS.md 第 11 章)。
package main

import (
	"fmt"
	"os"
)

// 以下变量由 ldflags 注入,见 Makefile 的 LDFLAGS。
var (
	// version 是发布版本,取自 git describe。
	version = "dev"
	// commit 是构建时的提交哈希。
	commit = "unknown"
	// buildTime 是构建时间(UTC)。
	buildTime = "unknown"
)

func main() {
	root := newRootCmd()

	if err := root.Execute(); err != nil {
		// cobra 已负责打印用法类错误;此处只补一条退出码。
		fmt.Fprintln(os.Stderr, "mesh:", err)
		os.Exit(1)
	}
}
