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

	// DefaultIngressMode 是默认接入模式:优先 TUN,失败自动降级。
	DefaultIngressMode = "auto"
	// DefaultIngressInterface 是默认 TUN 接口名。
	DefaultIngressInterface = "mesh0"

	// DefaultRoutingStrategy 是默认选路策略。
	DefaultRoutingStrategy = "shortest"
	// DefaultLSAFloodInterval 是默认 LSA 泛洪周期。
	DefaultLSAFloodInterval = 30 * time.Second
	// DefaultLSAMaxAge 是默认 LSA 失效时间。
	DefaultLSAMaxAge = 300 * time.Second
	// DefaultProbeInterval 是默认链路探测周期。
	DefaultProbeInterval = 1 * time.Second
	// DefaultEWMAAlpha 是默认延迟平滑系数。
	DefaultEWMAAlpha = 0.2
	// DefaultHeartbeat 是默认心跳周期。
	DefaultHeartbeat = 5 * time.Second
	// DefaultStaleTimeout 是默认陈旧超时。
	DefaultStaleTimeout = 20 * time.Second

	// DefaultKeyRotationInterval 是密钥轮换的默认周期。
	DefaultKeyRotationInterval = 168 * time.Hour
	// DefaultKeyRotationOverlap 是密钥轮换时新旧密钥的默认重叠窗口。
	DefaultKeyRotationOverlap = 24 * time.Hour
	// DefaultACLDefault 是 ACL 的默认动作。
	DefaultACLDefault = "allow"
	// DefaultAuditPath 是审计日志的默认路径。
	DefaultAuditPath = "/var/lib/mesh/audit.log"
)

// [security] 段相关取值范围(docs/CONFIG.md 6.8)。
const (
	// MinKeyRotationInterval 是密钥轮换周期的下限。
	MinKeyRotationInterval = time.Hour
	// MaxKeyRotationInterval 是密钥轮换周期的上限。
	MaxKeyRotationInterval = 8760 * time.Hour
	// MaxKeyRotationOverlap 是密钥重叠窗口的上限(下限为 0)。
	MaxKeyRotationOverlap = 720 * time.Hour
)

// ACL 动作取值。
const (
	// ACLAllow 表示允许匹配的流量。
	ACLAllow = "allow"
	// ACLDeny 表示拒绝匹配的流量。
	ACLDeny = "deny"
)

// ACLRule 是一条访问控制规则。
//
// 规则按**顺序**匹配,首条命中即生效(docs/CONFIG.md 6.8)。由于数组在合并
// 时采用追加语义,基础规则在前、追加规则在后,追加规则无法覆盖基础规则。
type ACLRule struct {
	// Action 是命中该规则时执行的动作:allow / deny。
	Action string `koanf:"action"`
	// From 是源地址或网段,如 10.126.126.0/24。
	From string `koanf:"from"`
	// To 是目的地址或网段。
	To string `koanf:"to"`
	// Ports 是允许/拒绝的端口列表;为空表示任意端口。
	Ports []int `koanf:"ports"`
	// Proto 是协议:tcp / udp / icmp / any;为空表示任意协议。
	Proto string `koanf:"proto"`
}

// KeyRotationConfig 描述密钥轮换策略。
type KeyRotationConfig struct {
	// Interval 是密钥轮换周期,范围 [1h, 8760h]。
	Interval time.Duration `koanf:"interval"`
	// Overlap 是轮换时新旧密钥的重叠窗口,范围 [0, 720h]。
	Overlap time.Duration `koanf:"overlap"`
}

// ACLConfig 描述访问控制策略。
type ACLConfig struct {
	// Default 是未命中任何规则时的默认动作:allow / deny。
	Default string `koanf:"default"`
	// Rules 是顺序匹配的规则列表。
	Rules []ACLRule `koanf:"rules"`
}

// AuditConfig 描述审计日志配置。
type AuditConfig struct {
	// Enabled 表示是否启用审计日志。
	Enabled bool `koanf:"enabled"`
	// Path 是审计日志文件路径。
	Path string `koanf:"path"`
}

// SecurityConfig 描述加密、访问控制与密钥轮换。
//
// 字段定义与取值见 docs/CONFIG.md 6.8,该文档是配置契约。
type SecurityConfig struct {
	// E2EE 表示是否启用端到端加密。关闭后中继可解密流量,仅用于调试。
	E2EE bool `koanf:"e2ee"`
	// KeyRotation 描述密钥轮换策略。
	KeyRotation KeyRotationConfig `koanf:"key_rotation"`
	// ACL 描述访问控制策略。
	ACL ACLConfig `koanf:"acl"`
	// Audit 描述审计日志配置。
	Audit AuditConfig `koanf:"audit"`
}

// validACLActions 是合法的 ACL 动作取值。
var validACLActions = []string{ACLAllow, ACLDeny}

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
	// Security 是加密、访问控制与密钥轮换配置。
	Security SecurityConfig `koanf:"security"`
	// Ingress 是接入层配置。
	Ingress IngressConfig `koanf:"ingress"`
	// TUN 是 TUN 设备配置。
	TUN TUNConfig `koanf:"tun"`
	// Routing 是路由与路径选择配置。
	Routing RoutingConfig `koanf:"routing"`
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

// IngressConfig 描述接入层配置(docs/CONFIG.md 6.4)。
type IngressConfig struct {
	// Mode 是接入模式:tun / no-tun / auto。
	Mode string `koanf:"mode"`
	// Interface 是 TUN 接口名;留空则使用系统默认或系统分配。
	//
	// macOS 的 utun 由系统分配名字,此字段必须留空。
	Interface string `koanf:"interface"`
}

// TUNConfig 描述 TUN 设备配置。
//
// 与 IngressConfig.Interface 的关系:两者都能指定接口名,Name 优先。
// 保留两个入口是为了兼容 docs/CONFIG.md 6.4 的 [ingress].interface 与
// 更直观的 [tun].name 两种写法。
type TUNConfig struct {
	// Name 是 TUN 接口名;留空则回退到 ingress.interface,再回退到默认值。
	Name string `koanf:"name"`
}

// RoutingConfig 描述路由与路径选择(docs/CONFIG.md 6.7)。
//
// 字段命名以本 Phase 的实现为准;CONFIG.md 6.7 用 lsa_interval /
// keepalive_interval / peer_degraded_after 等表达相近概念,后续阶段会统一。
type RoutingConfig struct {
	// Strategy 是选路策略:shortest / latency / weighted。
	Strategy string `koanf:"strategy"`
	// EWMAAlpha 是延迟平滑系数,范围 (0,1]。
	EWMAAlpha float64 `koanf:"ewma_alpha"`
	// LSAFloodInterval 是 LSA 周期泛洪间隔。
	LSAFloodInterval time.Duration `koanf:"lsa_flood_interval"`
	// LSAMaxAge 是 LSA 失效时间。
	LSAMaxAge time.Duration `koanf:"lsa_max_age"`
	// ProbeInterval 是链路质量探测周期。
	ProbeInterval time.Duration `koanf:"probe_interval"`
	// Heartbeat 是心跳周期。
	Heartbeat time.Duration `koanf:"heartbeat"`
	// StaleTimeout 是判定链路陈旧的超时。
	StaleTimeout time.Duration `koanf:"stale_timeout"`
}

// validRoutingStrategies 是合法的选路策略取值。
var validRoutingStrategies = []string{"shortest", "latency", "weighted"}

// 接入模式取值。
const (
	// IngressModeTUN 表示使用内核 TUN 设备(需要 CAP_NET_ADMIN / root)。
	IngressModeTUN = "tun"
	// IngressModeNoTUN 表示使用用户态协议栈(Phase 3 实现)。
	IngressModeNoTUN = "no-tun"
	// IngressModeAuto 表示优先 TUN,失败时按可用能力降级。
	IngressModeAuto = "auto"
)

// validIngressModes 是合法的接入模式取值。
var validIngressModes = []string{IngressModeTUN, IngressModeNoTUN, IngressModeAuto}

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
		Security: SecurityConfig{
			E2EE: true,
			KeyRotation: KeyRotationConfig{
				Interval: DefaultKeyRotationInterval,
				Overlap:  DefaultKeyRotationOverlap,
			},
			ACL: ACLConfig{
				Default: DefaultACLDefault,
				Rules:   []ACLRule{},
			},
			Audit: AuditConfig{
				Enabled: false,
				Path:    DefaultAuditPath,
			},
		},
		Ingress: IngressConfig{
			Mode:      DefaultIngressMode,
			Interface: DefaultIngressInterface,
		},
		TUN: TUNConfig{},
		Routing: RoutingConfig{
			Strategy:         DefaultRoutingStrategy,
			EWMAAlpha:        DefaultEWMAAlpha,
			LSAFloodInterval: DefaultLSAFloodInterval,
			LSAMaxAge:        DefaultLSAMaxAge,
			ProbeInterval:    DefaultProbeInterval,
			Heartbeat:        DefaultHeartbeat,
			StaleTimeout:     DefaultStaleTimeout,
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

	if err := c.Security.validate(); err != nil {
		return fmt.Errorf("config: validate: security: %w", err)
	}

	if !contains(validIngressModes, c.Ingress.Mode) {
		return fmt.Errorf("config: validate: ingress.mode %q not in %v", c.Ingress.Mode, validIngressModes)
	}

	if err := c.Routing.validate(); err != nil {
		return fmt.Errorf("config: validate: routing: %w", err)
	}

	return nil
}

// validate 校验 [routing] 段的取值范围(docs/CONFIG.md 6.7)。
func (r RoutingConfig) validate() error {
	if !contains(validRoutingStrategies, r.Strategy) {
		return fmt.Errorf("strategy %q not in %v", r.Strategy, validRoutingStrategies)
	}
	if r.EWMAAlpha <= 0 || r.EWMAAlpha > 1 {
		return fmt.Errorf("ewma_alpha %v out of range (0, 1]", r.EWMAAlpha)
	}
	if r.LSAFloodInterval <= 0 {
		return fmt.Errorf("lsa_flood_interval must be positive, got %v", r.LSAFloodInterval)
	}
	if r.LSAMaxAge <= 0 {
		return fmt.Errorf("lsa_max_age must be positive, got %v", r.LSAMaxAge)
	}
	if r.LSAMaxAge < r.LSAFloodInterval {
		return fmt.Errorf("lsa_max_age %v must not be shorter than lsa_flood_interval %v",
			r.LSAMaxAge, r.LSAFloodInterval)
	}
	if r.ProbeInterval <= 0 {
		return fmt.Errorf("probe_interval must be positive, got %v", r.ProbeInterval)
	}
	return nil
}

// EffectiveTUNName 返回最终使用的 TUN 接口名。
//
// 优先级:tun.name > ingress.interface > 默认值。留空表示由系统分配
// (macOS 的 utun* 即属此类)。
func (c *Config) EffectiveTUNName() string {
	if c == nil {
		return ""
	}
	if c.TUN.Name != "" {
		return c.TUN.Name
	}
	if c.Ingress.Interface != "" {
		return c.Ingress.Interface
	}
	return DefaultIngressInterface
}

// validate 校验 [security] 段的取值范围(docs/CONFIG.md 6.8)。
func (s SecurityConfig) validate() error {
	if !contains(validACLActions, s.ACL.Default) {
		return fmt.Errorf("acl.default %q not in %v", s.ACL.Default, validACLActions)
	}

	if s.KeyRotation.Interval < MinKeyRotationInterval || s.KeyRotation.Interval > MaxKeyRotationInterval {
		return fmt.Errorf("key_rotation.interval %v out of range [%v, %v]",
			s.KeyRotation.Interval, MinKeyRotationInterval, MaxKeyRotationInterval)
	}
	if s.KeyRotation.Overlap < 0 || s.KeyRotation.Overlap > MaxKeyRotationOverlap {
		return fmt.Errorf("key_rotation.overlap %v out of range [0, %v]",
			s.KeyRotation.Overlap, MaxKeyRotationOverlap)
	}
	// 重叠窗口不应超过轮换周期,否则新旧密钥永远无法结束共存。
	if s.KeyRotation.Overlap > s.KeyRotation.Interval {
		return fmt.Errorf("key_rotation.overlap %v must not exceed interval %v",
			s.KeyRotation.Overlap, s.KeyRotation.Interval)
	}

	for i, r := range s.ACL.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("acl.rules[%d]: %w", i, err)
		}
	}

	if s.Audit.Enabled && s.Audit.Path == "" {
		return fmt.Errorf("audit.path must not be empty when audit is enabled")
	}
	return nil
}

// validate 校验单条 ACL 规则。
func (r ACLRule) validate() error {
	if !contains(validACLActions, r.Action) {
		return fmt.Errorf("action %q not in %v", r.Action, validACLActions)
	}
	for _, p := range r.Ports {
		if p < 0 || p > 65535 {
			return fmt.Errorf("port %d out of range [0, 65535]", p)
		}
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

	// [security].acl.rules 是唯一的嵌套切片,必须深拷贝,否则快照与原件共享
	// 底层数组,热重载时会出现"改了一份、两份都变"的隐性别名。
	cp.Security.ACL.Rules = make([]ACLRule, len(c.Security.ACL.Rules))
	for i, r := range c.Security.ACL.Rules {
		rc := r
		rc.Ports = append([]int(nil), r.Ports...)
		cp.Security.ACL.Rules[i] = rc
	}
	return &cp
}
