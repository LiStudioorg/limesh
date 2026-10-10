package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	gotoml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/pflag"
)

// CLISection 是配置文件内嵌的 [cli] 块键名。
//
// 该块允许在配置文件里写 flag 语法,使其优先级位于"文件字段"之上、
// "环境变量"之下。完整语义见 AGENTS.md 第 10.3 节。
const CLISection = "cli"

// IncludeKey 是声明其它配置文件的顶层字段名。
const IncludeKey = "include"

// Options 控制加载行为。
type Options struct {
	// Paths 是 -c/--config 传入的路径,可重复,支持通配符。
	Paths []string
	// Flags 是命令行 pflag 集合,为空时跳过 CLI 层。
	Flags *pflag.FlagSet
	// EnvPrefix 是环境变量前缀,为空时使用 EnvPrefix 常量。
	EnvPrefix string
	// NoDefault 为真时跳过默认值层(仅用于测试)。
	NoDefault bool
}

// Load 按四层优先级加载并合并配置:
//
//	默认值 < 文件字段 < [cli] 块 < 环境变量(ENV) < 命令行(CLI)
//
// 返回的 Config 已完成校验。
func Load(opts Options) (*Config, error) {
	k := koanf.New(".")

	// ---- 第 1 层:默认值 ----
	if !opts.NoDefault {
		if err := k.Load(confmap.Provider(toMap(Default()), "."), nil); err != nil {
			return nil, fmt.Errorf("config: load defaults: %w", err)
		}
	}

	// ---- 第 2 层:文件字段 ----
	files, err := ExpandPaths(opts.Paths)
	if err != nil {
		return nil, err
	}
	merged, err := loadFiles(files)
	if err != nil {
		return nil, err
	}
	if len(merged) > 0 {
		if err := k.Load(confmap.Provider(merged, "."), nil); err != nil {
			return nil, fmt.Errorf("config: load files: %w", err)
		}
	}

	// ---- 第 3 层:内嵌 [cli] 块 ----
	if cli, ok := merged[CLISection].(map[string]any); ok && len(cli) > 0 {
		if err := k.Load(confmap.Provider(normalizeCLIBlock(cli), "."), nil); err != nil {
			return nil, fmt.Errorf("config: load [cli] block: %w", err)
		}
	}

	// ---- 第 4 层:环境变量 ----
	prefix := opts.EnvPrefix
	if prefix == "" {
		prefix = EnvPrefix
	}
	if err := k.Load(env.Provider(".", env.Opt{
		Prefix:        prefix,
		TransformFunc: envTransform,
	}), nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}

	// ---- 第 5 层:命令行 ----
	if opts.Flags != nil {
		if err := k.Load(posflag.ProviderWithFlag(opts.Flags, ".", k, flagTransform), nil); err != nil {
			return nil, fmt.Errorf("config: load flags: %w", err)
		}
	}

	cfg := &Config{}
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("config: unmarshal merged config: %w", err)
	}
	cfg.Path = strings.Join(files, string(os.PathListSeparator))

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadFiles 按给定顺序读取并合并文件。
//
// 每个文件的 reset 在该文件参与合并前生效,因此 reset 只作用于它之前
// 已合并的结果,不会影响更高优先级来源。
func loadFiles(paths []string) (map[string]any, error) {
	merged := map[string]any{}
	visited := map[string]bool{}

	for _, p := range paths {
		if err := loadOne(p, merged, visited); err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// loadOne 读取单个文件,处理其 include 后把自身合并进 merged。
//
// include 在自身字段之前处理,使被包含文件构成更低优先级的基底。
func loadOne(path string, merged map[string]any, visited map[string]bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("config: resolve %q: %w", path, err)
	}
	if visited[abs] {
		// 重复 include 同一文件只生效一次(按解析后的绝对路径去重)。
		return nil
	}
	visited[abs] = true

	raw, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("config: read %q: %w", abs, err)
	}

	var doc map[string]any
	if err := gotoml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("config: parse %q: %w", abs, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	// ---- 处理 include:相对路径基于当前文件所在目录 ----
	if includes := extractIncludes(doc); len(includes) > 0 {
		dir := filepath.Dir(abs)
		resolved := make([]string, 0, len(includes))
		for _, inc := range includes {
			if !filepath.IsAbs(inc) {
				inc = filepath.Join(dir, inc)
			}
			resolved = append(resolved, inc)
		}

		expanded, err := ExpandPaths(resolved)
		if err != nil {
			return fmt.Errorf("config: %q include: %w", abs, err)
		}
		for _, inc := range expanded {
			incAbs, err := filepath.Abs(inc)
			if err != nil {
				return fmt.Errorf("config: resolve include %q: %w", inc, err)
			}
			if err := loadOne(incAbs, merged, visited); err != nil {
				return fmt.Errorf("config: %q includes %q: %w", abs, incAbs, err)
			}
		}
	}

	// ---- 执行 reset 并合并自身 ----
	next, _ := mergeWithReset(merged, doc)
	for k, v := range next {
		merged[k] = v
	}
	return nil
}

// extractIncludes 取出 include 声明并归一为字符串切片。
func extractIncludes(doc map[string]any) []string {
	raw, ok := doc[IncludeKey]
	if !ok {
		return nil
	}

	var out []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, v...)
	case string:
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ExpandPaths 展开配置路径列表。
//
// 规则:
//   - 支持 `*`(匹配单层)与 `**`(匹配多层)
//   - 通配符展开结果按字典序排序,保证不同机器上顺序一致
//   - 无通配符的路径按给定顺序保留
//   - 匹配不到任何文件的通配符返回错误,避免静默丢配置
//   - 循环 include 由 loadOne 的 visited 检测,不会无限递归
func ExpandPaths(patterns []string) ([]string, error) {
	var out []string

	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		// 无通配符:直接使用(存在性由读取阶段报错)。
		if !strings.ContainsAny(pat, "*?[") {
			out = append(out, pat)
			continue
		}

		matches, err := glob(pat)
		if err != nil {
			return nil, fmt.Errorf("config: expand %q: %w", pat, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("config: expand %q: no files matched", pat)
		}
		out = append(out, matches...)
	}

	return out, nil
}

// glob 展开单个通配符模式,支持 ** 递归匹配。
//
// 标准库 filepath.Glob 不支持 **,因此在含 ** 时退化为手工递归遍历。
func glob(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		return matches, nil
	}

	// 以 ** 为界拆成前缀目录与后缀模式。
	idx := strings.Index(pattern, "**")
	root := pattern[:idx]
	suffix := strings.TrimPrefix(pattern[idx+2:], string(filepath.Separator))

	// 去掉 root 末尾的分隔符,得到递归起点。
	root = strings.TrimSuffix(root, string(filepath.Separator))
	if root == "" {
		root = "."
	}

	var matches []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if suffix == "" {
			matches = append(matches, path)
			return nil
		}
		ok, err := filepath.Match(suffix, filepath.Base(path))
		if err != nil {
			return err
		}
		if ok {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(matches)
	return matches, nil
}

// envTransform 把 MESH_NETWORK_MTU 这类环境变量映射为 network.mtu 路径。
//
// 规则:MESH_ 前缀之后的段全部小写,以 . 连接;
// 同时支持短形式 MESH_LOG → log(用于映射到嵌套字段时由 koanf 处理)。
func envTransform(key, value string) (string, any) {
	// provider 已按 Opt.Prefix 过滤并剥离前缀;此处再兜底一次。
	k := strings.TrimPrefix(key, EnvPrefix)
	k = strings.ToLower(k)
	k = strings.ReplaceAll(k, "__", ".")
	k = strings.ReplaceAll(k, "_", ".")

	// 短形式归一化:文档约定 MESH_LOG 等价于 MESH_LOG_LEVEL。
	// 双下划线可用于强制生成字面量下划线,此处不做转换。
	if alias, ok := envShortAliases[k]; ok {
		k = alias
	}
	return k, value
}

// envShortAliases 定义环境变量的短形式别名。
//
// 文档(AGENTS.md 2 章)约定 MESH_LOG 是 MESH_LOG_LEVEL 的短形式。
// 仅映射那些确有短形式的键,避免猜测带来的歧义。
var envShortAliases = map[string]string{
	"log": "log.level",
}

// flagTransform 把 flag 名映射为配置路径。
//
// 只有显式传入的 flag 参与合并;未传入的 flag 不覆盖文件配置。
func flagTransform(f *pflag.Flag) (string, any) {
	if f.Name == "config" {
		// -c 由 Load 的 Paths 参数处理,不进入配置树。
		return "", nil
	}
	return strings.ReplaceAll(f.Name, "-", "."), f.Value.String()
}

// toMap 把 Config 转换为 koanf 可消费的嵌套 map。
//
// 键名取自 koanf 标签而非结构体字段名:两者若不一致,默认值会以另一套键
// 落入配置树,与其它来源永不合并——这是一个静默且难以排查的错误。
func toMap(c *Config) map[string]any {
	if c == nil {
		return map[string]any{}
	}

	listen := make([]any, 0, len(c.Listen))
	for _, l := range c.Listen {
		listen = append(listen, map[string]any{"addr": l.Addr})
	}

	peers := make([]any, 0, len(c.Peers))
	for _, p := range c.Peers {
		peers = append(peers, p)
	}

	rules := make([]any, 0, len(c.Security.ACL.Rules))
	for _, r := range c.Security.ACL.Rules {
		ports := make([]any, 0, len(r.Ports))
		for _, p := range r.Ports {
			ports = append(ports, p)
		}
		rules = append(rules, map[string]any{
			"action": r.Action,
			"from":   r.From,
			"to":     r.To,
			"ports":  ports,
			"proto":  r.Proto,
		})
	}

	return map[string]any{
		"network": map[string]any{
			"name":     c.Network.Name,
			"secret":   c.Network.Secret,
			"hostname": c.Network.Hostname,
			"subnet":   c.Network.Subnet,
			"mtu":      c.Network.MTU,
		},
		"listen": listen,
		"peers":  peers,
		"log": map[string]any{
			"level":  c.Log.Level,
			"format": c.Log.Format,
		},
		"security": map[string]any{
			"e2ee": c.Security.E2EE,
			"key_rotation": map[string]any{
				// 以字符串形式给出,koanf 会解成 time.Duration;
				// 若给整数会被当作纳秒,与 TOML 中的 "168h" 语义不一致。
				"interval": c.Security.KeyRotation.Interval.String(),
				"overlap":  c.Security.KeyRotation.Overlap.String(),
			},
			"acl": map[string]any{
				"default": c.Security.ACL.Default,
				"rules":   rules,
			},
			"audit": map[string]any{
				"enabled": c.Security.Audit.Enabled,
				"path":    c.Security.Audit.Path,
			},
		},
	}
}

// 以下导入在部分构建路径下由 koanf 间接使用,显式保留以便替换实现。

// ErrNoConfig 表示未提供任何配置文件。
var ErrNoConfig = errors.New("config: no configuration file provided")

// normalizeCLIBlock 把 [cli] 块中的标量写法展开为嵌套字段路径。
//
// [cli] 块允许写 flag 语法,而 flag 名是扁平的:
//
//	[cli]
//	log    = "debug"     → log.level = "debug"
//	topo   = true        → 顶层布尔,保持不变
//
// 若不展开,标量 log 会覆盖 map 形态的 [log] 表,导致解码失败。
// 已知的"结构体字段 ↔ 扁平 flag"映射在此显式声明。
func normalizeCLIBlock(cli map[string]any) map[string]any {
	out := make(map[string]any, len(cli))

	for k, v := range cli {
		// log 是嵌套结构;标量写法按文档约定指向 log.level。
		if k == "log" {
			if _, isMap := v.(map[string]any); !isMap {
				setPath(out, "log.level", v)
				continue
			}
		}
		// 键名带点号时已显式指明路径,按原样写入。
		if strings.Contains(k, ".") {
			setPath(out, k, v)
			continue
		}
		out[k] = v
	}
	return out
}
