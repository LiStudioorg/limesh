package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFile 在 dir 下写入文件,自动创建父目录。
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// clearEnv 清空 MESH_* 环境变量,避免宿主机环境污染测试。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				key := kv[:i]
				if len(key) >= len(EnvPrefix) && key[:len(EnvPrefix)] == EnvPrefix {
					t.Setenv(key, "")
					require.NoError(t, os.Unsetenv(key))
				}
				break
			}
		}
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	require.NoError(t, cfg.Validate(), "默认值必须自洽")
	assert.Equal(t, DefaultNetworkName, cfg.Network.Name)
	assert.Equal(t, DefaultMTU, cfg.Network.MTU)
	assert.Equal(t, DefaultSubnet, cfg.Network.Subnet)
	assert.Len(t, cfg.Listen, 1)
	assert.Equal(t, "udp", cfg.Listen[0].Scheme())
	assert.Equal(t, "0.0.0.0:11010", cfg.Listen[0].HostPort())
}

func TestLoadSingleFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[network]
name = "prod"
hostname = "node-a"

[log]
level = "debug"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)

	assert.Equal(t, "prod", cfg.Network.Name)
	assert.Equal(t, "node-a", cfg.Network.Hostname)
	assert.Equal(t, "debug", cfg.Log.Level)
	// 文件未提及的字段应保留默认值。
	assert.Equal(t, DefaultMTU, cfg.Network.MTU)
	assert.Equal(t, DefaultLogFormat, cfg.Log.Format)
}

func TestLoadMultipleFilesArrayAppends(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", `
peers = ["1.2.3.4:11010"]
`)
	over := writeFile(t, dir, "override.toml", `
peers = ["5.6.7.8:11010"]
`)

	cfg, err := Load(Options{Paths: []string{base, over}})
	require.NoError(t, err)

	assert.Equal(t, []string{"1.2.3.4:11010", "5.6.7.8:11010"}, cfg.Peers,
		"多文件数组应按顺序追加")
}

func TestLoadScalarOverride(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", "[network]\nmtu = 1380\n")
	over := writeFile(t, dir, "override.toml", "[network]\nmtu = 1400\n")

	cfg, err := Load(Options{Paths: []string{base, over}})
	require.NoError(t, err)
	assert.Equal(t, 1400, cfg.Network.MTU)
}

func TestLoadResetClearsInherited(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", `
peers = ["1.2.3.4:11010"]
`)
	over := writeFile(t, dir, "override.toml", `
reset = ["peers"]
peers = ["9.9.9.9:11010"]
`)

	cfg, err := Load(Options{Paths: []string{base, over}})
	require.NoError(t, err)

	assert.Equal(t, []string{"9.9.9.9:11010"}, cfg.Peers,
		"reset 应清空继承值,仅保留重新定义的项")
}

func TestLoadEmptyArrayDoesNotClear(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", `peers = ["1.2.3.4:11010"]`+"\n")
	over := writeFile(t, dir, "override.toml", `peers = []`+"\n")

	cfg, err := Load(Options{Paths: []string{base, over}})
	require.NoError(t, err)

	assert.Equal(t, []string{"1.2.3.4:11010"}, cfg.Peers,
		"裸写 [] 不清空(与 CONFIG.md 4.3 一致)")
}

func TestLoadIncludeRelativeToFileDir(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	// conf.d 与主配置同级;include 路径基于主配置所在目录解析。
	writeFile(t, dir, "conf.d/10-network.toml", "[network]\nname = \"included\"\n")
	main := writeFile(t, dir, "config.toml", `
include = ["conf.d/*.toml"]

[network]
hostname = "node-a"
`)

	cfg, err := Load(Options{Paths: []string{main}})
	require.NoError(t, err)

	assert.Equal(t, "included", cfg.Network.Name, "被 include 的文件应参与合并")
	assert.Equal(t, "node-a", cfg.Network.Hostname, "主文件字段应覆盖被包含文件")
}

func TestLoadIncludeRecursive(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	writeFile(t, dir, "a/b/deep.toml", "[log]\nlevel = \"error\"\n")
	main := writeFile(t, dir, "config.toml", `include = ["a/**/*.toml"]`+"\n")

	cfg, err := Load(Options{Paths: []string{main}})
	require.NoError(t, err)
	assert.Equal(t, "error", cfg.Log.Level, "** 应递归匹配多层目录")
}

func TestLoadIncludeCycleTerminates(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	// a 与 b 互相 include,应被去重而不无限递归。
	writeFile(t, dir, "a.toml", "include = [\"b.toml\"]\n[network]\nname = \"a\"\n")
	writeFile(t, dir, "b.toml", "include = [\"a.toml\"]\n[network]\nname = \"b\"\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		cfg, err := Load(Options{Paths: []string{filepath.Join(dir, "a.toml")}})
		assert.NoError(t, err)
		if cfg != nil {
			assert.Equal(t, "a", cfg.Network.Name, "主文件应最后合并")
		}
	}()

	select {
	case <-done:
	case <-timeAfter(t):
		t.Fatal("循环 include 未终止")
	}
}

func TestLoadCLIBlockOverridesFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[log]
level = "warn"

[cli]
log = "debug"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)

	assert.Equal(t, "debug", cfg.Log.Level, "[cli] 块优先级高于文件字段")
}

func TestLoadEnvOverridesCLIBlock(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[log]
level = "warn"

[cli]
log = "debug"
`)
	t.Setenv("MESH_LOG_LEVEL", "error")

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)

	assert.Equal(t, "error", cfg.Log.Level, "ENV 优先级高于 [cli] 块")
}

func TestLoadFlagsOverrideEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[log]
level = "warn"
`)
	t.Setenv("MESH_LOG_LEVEL", "error")

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("log.level", "", "log level")
	require.NoError(t, fs.Set("log.level", "debug"))

	cfg, err := Load(Options{Paths: []string{p}, Flags: fs})
	require.NoError(t, err)

	assert.Equal(t, "debug", cfg.Log.Level, "CLI 优先级最高")
}

func TestLoadFullPriorityChain(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	// 默认 info;文件 warn;[cli] debug;ENV error;CLI debug。
	p := writeFile(t, dir, "config.toml", `
[log]
level = "warn"

[cli]
log = "debug"
`)
	t.Setenv("MESH_LOG_LEVEL", "error")

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("log.level", "", "log level")
	require.NoError(t, fs.Set("log.level", "debug"))

	cfg, err := Load(Options{Paths: []string{p}, Flags: fs})
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.Log.Level, "CLI 层应覆盖 ENV 层的 error")

	// 逐层移除,验证每一层都真实参与合并。
	t.Setenv("MESH_LOG_LEVEL", "")
	require.NoError(t, os.Unsetenv("MESH_LOG_LEVEL"))
	cfg, err = Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.Log.Level, "去掉 ENV 与 CLI 后应回落到 [cli] 块")

	p2 := writeFile(t, dir, "no-cli.toml", "[log]\nlevel = \"warn\"\n")
	cfg, err = Load(Options{Paths: []string{p2}})
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.Log.Level, "无 [cli] 块时应回落到文件")

	cfg, err = Load(Options{Paths: []string{p2}})
	require.NoError(t, err)
	assert.NotEqual(t, DefaultLogLevel, cfg.Log.Level)
}

func TestLoadEnvShortForm(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", "[log]\nlevel = \"info\"\n")
	t.Setenv("MESH_LOG", "debug")

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)

	// MESH_LOG 是 MESH_LOG_LEVEL 的短形式,同样映射到 log.level。
	assert.Equal(t, "debug", cfg.Log.Level, "MESH_LOG 短形式应生效")
}

func TestLoadMissingFileErrors(t *testing.T) {
	clearEnv(t)
	_, err := Load(Options{Paths: []string{filepath.Join(t.TempDir(), "nope.toml")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

func TestLoadInvalidTOMLFails(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "bad.toml", "this is not = = toml\n")

	_, err := Load(Options{Paths: []string{p}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestLoadValidationFailure(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()

	cases := map[string]string{
		"empty name":   "[network]\nname = \"\"\n",
		"bad mtu":      "[network]\nmtu = 10\n",
		"bad subnet":   "[network]\nsubnet = \"not-a-cidr\"\n",
		"bad scheme":   "[[listen]]\naddr = \"sctp://0.0.0.0:11010\"\n",
		"bad port":     "[[listen]]\naddr = \"udp://0.0.0.0:99999\"\n",
		"no bracket":   "[[listen]]\naddr = \"0.0.0.0:11010\"\n",
		"bad peer":     "peers = [\"no-port\"]\n",
		"bad loglevel": "[log]\nlevel = \"loud\"\n",
		"bad logfmt":   "[log]\nformat = \"xml\"\n",
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, dir, name+".toml", content)
			_, err := Load(Options{Paths: []string{p}})
			assert.Error(t, err, "应校验失败")
		})
	}
}

func TestLoadSetsPath(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", "")

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, p, cfg.Path, "Path 应记录来源文件")
}

func TestExpandPathsPlainNoGlob(t *testing.T) {
	got, err := ExpandPaths([]string{"a.toml", "b.toml"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.toml", "b.toml"}, got, "无通配符应保持给定顺序")
}

func TestExpandPathsSingleLevelSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "conf.d/30-c.toml", "")
	writeFile(t, dir, "conf.d/10-a.toml", "")
	writeFile(t, dir, "conf.d/20-b.toml", "")

	got, err := ExpandPaths([]string{filepath.Join(dir, "conf.d", "*.toml")})
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, "10-a.toml", filepath.Base(got[0]), "通配符结果应字典序排序")
	assert.Equal(t, "20-b.toml", filepath.Base(got[1]))
	assert.Equal(t, "30-c.toml", filepath.Base(got[2]))
}

func TestExpandPathsRecursiveSorted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "c/x.toml", "")
	writeFile(t, dir, "a/y.toml", "")
	writeFile(t, dir, "b/z.toml", "")

	got, err := ExpandPaths([]string{filepath.Join(dir, "**", "*.toml")})
	require.NoError(t, err)

	require.Len(t, got, 3)
	assert.Equal(t, "y.toml", filepath.Base(got[0]))
	assert.Equal(t, "z.toml", filepath.Base(got[1]))
	assert.Equal(t, "x.toml", filepath.Base(got[2]))
}

func TestExpandPathsNoMatchErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := ExpandPaths([]string{filepath.Join(dir, "*.nope")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no files matched")
}

func TestExpandPathsSkipsEmpty(t *testing.T) {
	got, err := ExpandPaths([]string{"", "a.toml"})
	require.NoError(t, err)
	assert.Equal(t, []string{"a.toml"}, got)
}

func TestGlobRecursiveMatchesBasename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "deep/nested/only.toml", "")
	writeFile(t, dir, "deep/other.txt", "")

	got, err := glob(filepath.Join(dir, "**", "*.toml"))
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Contains(t, got[0], "only.toml")
}

func TestExtractIncludesVariants(t *testing.T) {
	assert.Equal(t, []string{"a", "b"},
		extractIncludes(map[string]any{"include": []any{"a", "b"}}))
	assert.Equal(t, []string{"solo"},
		extractIncludes(map[string]any{"include": "solo"}))
	assert.Nil(t, extractIncludes(map[string]any{"include": []any{1, ""}}))
	assert.Nil(t, extractIncludes(map[string]any{}))
}

func TestToMapRoundTrip(t *testing.T) {
	m := toMap(Default())

	net, ok := m["network"].(map[string]any)
	require.True(t, ok, "应生成嵌套结构")
	assert.Equal(t, DefaultNetworkName, net["name"])

	listen, ok := m["listen"].([]any)
	require.True(t, ok)
	require.Len(t, listen, 1)
	assert.Equal(t, "udp://0.0.0.0:11010",
		listen[0].(map[string]any)["addr"])
}

func TestConfigSnapshotIsDeepCopy(t *testing.T) {
	cfg := Default()
	cp := cfg.Snapshot()

	cp.Network.Name = "changed"
	cp.Listen[0].Addr = "tcp://0.0.0.0:1"
	cp.Peers = append(cp.Peers, "1.2.3.4:11010")

	assert.Equal(t, DefaultNetworkName, cfg.Network.Name, "Network 应独立")
	assert.Equal(t, "udp://0.0.0.0:11010", cfg.Listen[0].Addr, "Listen 切片应独立")
	assert.Empty(t, cfg.Peers, "Peers 切片应独立")
}

func TestValidateNilConfig(t *testing.T) {
	var c *Config
	assert.Error(t, c.Validate())
}

func TestListenConfigAccessors(t *testing.T) {
	l := ListenConfig{Addr: "udp://0.0.0.0:11010"}
	assert.Equal(t, "udp", l.Scheme())
	assert.Equal(t, "0.0.0.0:11010", l.HostPort())

	bare := ListenConfig{Addr: "0.0.0.0:11010"}
	assert.Equal(t, "", bare.Scheme())
	assert.Equal(t, "0.0.0.0:11010", bare.HostPort())
}

func TestHostnameOr(t *testing.T) {
	cfg := Default()
	assert.Equal(t, "fallback", cfg.HostnameOr("fallback"))

	cfg.Network.Hostname = "node-a"
	assert.Equal(t, "node-a", cfg.HostnameOr("fallback"))
}
