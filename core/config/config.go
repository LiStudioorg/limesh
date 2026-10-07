// Package config 实现 Limesh 的配置加载与合并。
//
// 配置系统必须同时满足:人类可读、机器可覆盖、集群可组合、运行时可热更。
// 合并顺序(后者覆盖前者)为:
//
//	默认值 < 文件字段 < 内嵌 [cli] 块 < 环境变量(ENV) < 命令行(CLI)
//
// 数组默认"追加"而非替换;需要清空继承值时必须使用顶层 reset 字段,
// 裸写 [] 只追加零个元素,不清空。
//
// 设计说明见 AGENTS.md 第 10 章,配置项参考见 docs/CONFIG.md。
package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// 默认值,取值见 AGENTS.md 第 8.11 节协议常量。
const (
	// DefaultPort 是默认监听端口(UDP/TCP 共用)。
	DefaultPort = 11010
	// DefaultSubnet 是默认虚拟网段。
	DefaultSubnet = "10.126.126.0/24"
	// DefaultMTU 是虚拟 MTU,为 ZCPacket 头与传输层封装预留空间。
	DefaultMTU = 1380
	// DefaultLogLevel 是默认日志级别。
	DefaultLogLevel = "info"
	// DefaultLogFormat 是默认日志格式。
	DefaultLogFormat = "console"
	// DefaultNetworkName 是默认网络名称。
	DefaultNetworkName = "mesh"
	// EnvPrefix 是环境变量前缀。
	EnvPrefix = "MESH_"
)

// 支持的日志级别与格式。
var (
	validLogLevels  = []string{"debug", "info", "warn", "error"}
	validLogFormats = []string{"console", "json"}
	validSchemes    = []string{"udp", "tcp"}
)

// Config 是合并后的配置快照。
//
// Phase 0 只实现网络身份、监听、对端与日志;其余模块的字段在后续阶段加入。
// 运行期所有模块只读该快照,不允许各自读取环境变量。
type Config struct {
	// Network 描述网络身份。
	Network NetworkConfig `koanf:"network"`
	// Listen 是本节点的监听地址列表。
	Listen []ListenConfig `koanf:"listen"`
	// Peers 是静态对端列表,元素格式为 host:port。
	Peers []string `koanf:"peers"`
	// Log 是日志配置。
	Log LogConfig `koanf:"log"`
	// Path 记录配置来源文件,供排错与来源追踪使用,不参与序列化。
	Path string `koanf:"-"`
}

// NetworkConfig 描述网络身份。
type NetworkConfig struct {
	// Name 是网络名称,同一网络内的节点必须一致。
	Name string `koanf:"name"`
	// Secret 是网络密钥。生产环境应置于 0600 的 secrets/*.toml。
	Secret string `koanf:"secret"`
	// Hostname 是本节点主机名,留空则取系统 hostname。
	Hostname string `koanf:"hostname"`
	// Subnet 是虚拟网段。
	Subnet string `koanf:"subnet"`
	// MTU 是虚拟 MTU。
	MTU int `koanf:"mtu"`
}

// ListenConfig 描述一个监听地址。
type ListenConfig struct {
	// Addr 是监听地址,格式为 scheme://host:port。
	Addr string `koanf:"addr"`
}

// Scheme 返回监听地址的协议方案(udp / tcp)。
func (l ListenConfig) Scheme() string {
	if i := strings.Index(l.Addr, "://"); i > 0 {
		return l.Addr[:i]
	}
	return ""
}

// HostPort 返回监听地址的 host:port 部分。
func (l ListenConfig) HostPort() string {
	if i := strings.Index(l.Addr, "://"); i >= 0 {
		return l.Addr[i+3:]
	}
	return l.Addr
}

// LogConfig 描述日志行为。
type LogConfig struct {
	// Level 是日志级别:debug / info / warn / error。
	Level string `koanf:"level"`
	// Format 是日志格式:console / json。
	Format string `koanf:"format"`
}

// Default 返回内置默认值构成的配置。
//
// 默认值是优先级链的最低层;任何来源都可以覆盖它。
func Default() *Config {
	return &Config{
		Network: NetworkConfig{
			Name:   DefaultNetworkName,
			Subnet: DefaultSubnet,
			MTU:    DefaultMTU,
		},
		Listen: []ListenConfig{
			{Addr: fmt.Sprintf("udp://0.0.0.0:%d", DefaultPort)},
		},
		Peers: []string{},
		Log: LogConfig{
			Level:  DefaultLogLevel,
			Format: DefaultLogFormat,
		},
	}
}

// Validate 校验配置的合法性与一致性。
//
// 校验在启动与热重载时都会执行;任一项不通过即拒绝加载。
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config: validate: nil config")
	}

	if c.Network.Name == "" {
		return fmt.Errorf("config: validate: network.name must not be empty")
	}
	if c.Network.MTU < 576 || c.Network.MTU > 65535 {
		return fmt.Errorf("config: validate: network.mtu %d out of range [576, 65535]", c.Network.MTU)
	}
	if c.Network.Subnet != "" {
		ip, ipnet, err := net.ParseCIDR(c.Network.Subnet)
		if err != nil {
			return fmt.Errorf("config: validate: network.subnet %q: %w", c.Network.Subnet, err)
		}
		if !ip.Equal(ipnet.IP) {
			return fmt.Errorf("config: validate: network.subnet %q is not a network address (want %s)",
				c.Network.Subnet, ipnet.String())
		}
	}

	if len(c.Listen) == 0 {
		return fmt.Errorf("config: validate: at least one [[listen]] entry is required")
	}
	for i, l := range c.Listen {
		if err := l.validate(); err != nil {
			return fmt.Errorf("config: validate: listen[%d]: %w", i, err)
		}
	}

	for i, p := range c.Peers {
		if err := validatePeer(p); err != nil {
			return fmt.Errorf("config: validate: peers[%d]: %w", i, err)
		}
	}

	if !contains(validLogLevels, c.Log.Level) {
		return fmt.Errorf("config: validate: log.level %q not in %v", c.Log.Level, validLogLevels)
	}
	if !contains(validLogFormats, c.Log.Format) {
		return fmt.Errorf("config: validate: log.format %q not in %v", c.Log.Format, validLogFormats)
	}

	return nil
}

// validate 校验单条监听地址。
func (l ListenConfig) validate() error {
	if l.Addr == "" {
		return fmt.Errorf("addr must not be empty")
	}
	scheme := l.Scheme()
	if scheme == "" {
		return fmt.Errorf("addr %q must be scheme://host:port (e.g. udp://0.0.0.0:11010)", l.Addr)
	}
	if !contains(validSchemes, scheme) {
		return fmt.Errorf("addr %q: unsupported scheme %q, want one of %v", l.Addr, scheme, validSchemes)
	}

	host, portStr, err := net.SplitHostPort(l.HostPort())
	if err != nil {
		return fmt.Errorf("addr %q: %w", l.Addr, err)
	}
	if host != "" && net.ParseIP(host) == nil {
		// Phase 0 只接受 IP 形式的监听地址;主机名解析留到后续阶段。
		return fmt.Errorf("addr %q: host %q must be an IP literal", l.Addr, host)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("addr %q: invalid port %q", l.Addr, portStr)
	}
	return nil
}

// validatePeer 校验一个静态对端地址。
func validatePeer(p string) error {
	if p == "" {
		return fmt.Errorf("peer must not be empty")
	}
	host, portStr, err := net.SplitHostPort(p)
	if err != nil {
		return fmt.Errorf("peer %q: want host:port: %w", p, err)
	}
	if host == "" {
		return fmt.Errorf("peer %q: host must not be empty", p)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("peer %q: invalid port %q", p, portStr)
	}
	return nil
}

// contains 判断 ss 是否包含 s。
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// HostnameOr 返回配置的主机名;为空时回退到 fallback。
//
// 该函数不读取系统 hostname,以保持 core 包对运行环境的零依赖;
// 系统 hostname 由调用方(CLI 层)传入。
func (c *Config) HostnameOr(fallback string) string {
	if c.Network.Hostname != "" {
		return c.Network.Hostname
	}
	return fallback
}

// Snapshot 返回配置的深拷贝,用于热重载时的原子快照替换。
func (c *Config) Snapshot() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Listen = append([]ListenConfig(nil), c.Listen...)
	cp.Peers = append([]string(nil), c.Peers...)
	return &cp
}

// 保留 time 导入:热重载与超时配置在后续阶段使用。
var _ = time.Second
