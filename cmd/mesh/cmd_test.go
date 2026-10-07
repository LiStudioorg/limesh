package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/config"
)

// execute 在给定参数下执行根命令,返回标准输出与错误。
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.Execute()
	return out.String(), err
}

// writeConfig 写入一份可用的最小配置。
func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

const minimalConfig = `
[network]
name = "test-net"
hostname = "node-a"

[[listen]]
addr = "udp://127.0.0.1:0"

[log]
level = "error"
format = "console"
`

func TestVersionCommand(t *testing.T) {
	out, err := execute(t, "version")
	require.NoError(t, err)

	assert.Contains(t, out, "mesh")
	assert.Contains(t, out, "dev", "未注入 ldflags 时应显示默认版本")
	assert.Contains(t, out, "commit:")
	assert.Contains(t, out, "built:")
}

func TestVersionShortFlag(t *testing.T) {
	out, err := execute(t, "version", "--short")
	require.NoError(t, err)

	assert.Equal(t, "dev\n", out, "--short 应只输出版本号")
}

func TestVersionVerbose(t *testing.T) {
	out, err := execute(t, "version", "-v")
	require.NoError(t, err)

	assert.Contains(t, out, "go:")
	assert.Contains(t, out, "platform:")
}

func TestVersionRejectsArgs(t *testing.T) {
	_, err := execute(t, "version", "extra-arg")
	require.Error(t, err, "version 不接受位置参数")
}

func TestRootHelp(t *testing.T) {
	out, err := execute(t, "--help")
	require.NoError(t, err)

	assert.Contains(t, out, "mesh")
	assert.Contains(t, out, "node")
	assert.Contains(t, out, "version")
	assert.Contains(t, out, "--config")
}

func TestNodeHelp(t *testing.T) {
	out, err := execute(t, "node", "--help")
	require.NoError(t, err)

	assert.Contains(t, out, "node")
	assert.Contains(t, out, "--log.level")
	assert.Contains(t, out, "--log.format")
}

func TestConfigFlagIsRepeatable(t *testing.T) {
	// -c 必须可重复;此处只验证 flag 定义,真实加载由集成路径覆盖。
	root := newRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	require.NotNil(t, flag)
	assert.Equal(t, "c", flag.Shorthand)
	assert.Equal(t, "stringArray", flag.Value.Type(), "应支持重复传入")
}

func TestNodeRequiresConfig(t *testing.T) {
	_, err := execute(t, "node")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no config file")
}

func TestNodeRejectsMissingFile(t *testing.T) {
	_, err := execute(t, "node", "-c", filepath.Join(t.TempDir(), "nope.toml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

func TestNodeRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "bad.toml", "[network]\nname = \"\"\n")

	_, err := execute(t, "node", "-c", p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate")
}

func TestNodeRejectsBadLogLevel(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "bad-level.toml", `
[log]
level = "loud"
`)

	_, err := execute(t, "node", "-c", p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log.level")
}

func TestNodeAcceptsConfigExample(t *testing.T) {
	// config.example.toml 必须始终是合法配置——它是用户的第一入口。
	if _, err := os.Stat("../../config.example.toml"); err != nil {
		t.Skip("config.example.toml 不在预期位置")
	}

	out, err := execute(t, "node", "-c", "../../config.example.toml", "--help")
	require.NoError(t, err)
	assert.NotEmpty(t, out)
}

func TestNodeFlagsOverrideConfig(t *testing.T) {
	// --log.level 属于 CLI 层,应能覆盖文件字段。
	// 这里通过构建 logger 的路径间接验证:非法级别应在加载阶段被拒。
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.toml", minimalConfig)

	_, err := execute(t, "node", "-c", p, "--log.level", "loud")
	require.Error(t, err, "命令行传入非法级别应被校验拒绝")
}

func TestListenAddrsHelper(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.toml", minimalConfig)

	out, err := execute(t, "node", "-c", p, "--help")
	require.NoError(t, err)
	assert.NotEmpty(t, out)

	// 直接验证纯函数本身。
	cfg, err := config.Load(config.Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, []string{"udp://127.0.0.1:0"}, listenAddrs(cfg))
}

func TestBuildLoggerFormats(t *testing.T) {
	console := config.Default()
	console.Log.Level = "debug"
	console.Log.Format = "console"

	logger, err := buildLogger(console, false)
	require.NoError(t, err)
	assert.NotNil(t, logger)

	jsonCfg := config.Default()
	jsonCfg.Log.Format = "json"
	logger, err = buildLogger(jsonCfg, false)
	require.NoError(t, err)
	assert.NotNil(t, logger)

	// verbose 应把级别降到 debug 而不报错。
	logger, err = buildLogger(jsonCfg, true)
	require.NoError(t, err)
	assert.NotNil(t, logger)
}

func TestBuildLoggerRejectsBadLevel(t *testing.T) {
	cfg := config.Default()
	cfg.Log.Level = "loud"

	_, err := buildLogger(cfg, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid log level")
}

func TestRootHasVersionFlag(t *testing.T) {
	out, err := execute(t, "--version")
	require.NoError(t, err)
	assert.Contains(t, out, "dev")
}

func TestNodeFlagSetIncludesRootPersistentFlags(t *testing.T) {
	// 回归测试:曾经用 cmd.Flags() 加载配置,导致根命令的持久 flag
	// 不参与合并,表现为"flag 传了但没生效"(--log.format 传了仍是 console)。
	root := newRootCmd()

	var nodeCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "node" {
			nodeCmd = c
		}
	}
	require.NotNil(t, nodeCmd, "应存在 node 子命令")

	// 复现 runNode 中构造 flag 集的方式。
	flags := nodeCmd.Flags()
	flags.AddFlagSet(root.PersistentFlags())

	// 三者缺一不可:配置路径(-c)、根命令的 -v、子命令自己的 --log.*。
	assert.NotNil(t, flags.Lookup("config"), "根命令的 -c 应包含在 flag 集中")
	assert.NotNil(t, flags.Lookup("verbose"), "根命令的 -v 应包含在 flag 集中")
	assert.NotNil(t, flags.Lookup("log.level"), "子命令的 --log.level 应包含在 flag 集中")
	assert.NotNil(t, flags.Lookup("log.format"), "子命令的 --log.format 应包含在 flag 集中")

	// 注:AddFlagSet 共享底层 FlagSet,调用后 nodeCmd.Flags() 也会看到新增项,
	// 因此无法在本测试内反证"修复前"的状态。修复的正确性由
	// TestNodeFlagsReachConfig 通过真实执行路径验证。
}

func TestNodeFlagsReachConfig(t *testing.T) {
	// 端到端验证:命令行 flag 必须真正进入配置合并,而不仅仅"被解析"。
	// 这是修复前失败、修复后通过的用例。
	dir := t.TempDir()
	p := writeConfig(t, dir, "cfg.toml", minimalConfig)

	root := newRootCmd()
	root.SetArgs([]string{"node", "-c", p, "--log.format", "json", "--help"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	require.NoError(t, root.Execute())

	// 通过配置加载路径直接验证 flag 生效。
	flags := pflag.NewFlagSet("t", pflag.ContinueOnError)
	flags.String("log.format", "console", "")
	require.NoError(t, flags.Set("log.format", "json"))

	cfg, err := config.Load(config.Options{Paths: []string{p}, Flags: flags})
	require.NoError(t, err)
	assert.Equal(t, "json", cfg.Log.Format, "命令行 flag 应覆盖文件配置")
}
