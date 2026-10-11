package nat

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
)

// CandidateType 是候选地址的类型。
type CandidateType string

// 候选地址类型取值。
const (
	// CandidateHost 是本地网卡地址。
	CandidateHost CandidateType = "host"
	// CandidateSrflx 是 STUN 反射地址(公网映射)。
	CandidateSrflx CandidateType = "srflx"
	// CandidateRelay 是中继地址(Phase 2c)。
	CandidateRelay CandidateType = "relay"
)

// ICE 风格优先级参数(简化版)。
//
// priority = (2^24)*type_pref + (2^8)*local_pref + (256 - component)
const (
	// typePrefHost / typePrefSrflx / typePrefRelay 是类型偏好。
	typePrefHost  = 126
	typePrefSrflx = 100
	typePrefRelay = 0

	// componentRTP 是唯一的传输组件(组网只有一个数据面组件)。
	componentRTP = 1
)

// Candidate 是一个候选地址。
type Candidate struct {
	// Type 是候选类型:host / srflx / relay。
	Type CandidateType `json:"type"`
	// Addr 是候选地址(IP:port)。
	Addr netip.AddrPort `json:"addr"`
	// Priority 是 ICE 风格优先级,数值越大越优先。
	Priority uint32 `json:"priority"`
	// Source 是来源:网卡名 / STUN 服务器 / relay 地址。
	Source string `json:"source,omitempty"`
}

// String 返回便于日志展示的形式。
func (c Candidate) String() string {
	return fmt.Sprintf("%s/%s (prio %d)", c.Type, c.Addr, c.Priority)
}

// ipPriority 计算一个地址的 local_pref。
//
// IPv6 全球单播 > IPv4 全球/公网 > IPv4 私有 > IPv4 链路本地 > IPv6 链路本地。
func ipPriority(ip netip.Addr) uint32 {
	switch {
	case ip.Is4():
		// IPv4:私有地址较低,其它(公网)较高。
		if ip.IsPrivate() {
			return 40
		}
		if ip.IsLinkLocalUnicast() {
			return 10
		}
		return 50
	default:
		// IPv6:全球单播最高。
		if ip.IsLinkLocalUnicast() {
			return 5
		}
		if ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return 60
		}
		return 30
	}
}

// priority 计算候选优先级。
func priority(t CandidateType, ip netip.Addr) uint32 {
	var typePref uint32
	switch t {
	case CandidateHost:
		typePref = typePrefHost
	case CandidateSrflx:
		typePref = typePrefSrflx
	case CandidateRelay:
		typePref = typePrefRelay
	}
	return (typePref << 24) | (ipPriority(ip) << 8) | (256 - componentRTP)
}

// Collector 收集本机候选地址。
type Collector struct {
	cfg  *config.Config
	stun *Detector
	log  *zap.Logger
	port uint16 // 数据面端口(默认取自配置)
}

// CollectorOptions 是构造 Collector 的参数。
type CollectorOptions struct {
	// Config 是配置快照,必填。
	Config *config.Config
	// Detector 是 NAT 探测器,可为 nil(则只收集 host 候选)。
	Detector *Detector
	// Logger 为日志实例;为空时使用 zap.NewNop()。
	Logger *zap.Logger
	// Port 是本地数据面端口;为 0 时从配置的 listen 推导。
	Port uint16
}

// NewCollector 构造候选地址收集器。
func NewCollector(opts CollectorOptions) (*Collector, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("nat: new collector: config must not be nil")
	}
	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &Collector{
		cfg:  opts.Config,
		stun: opts.Detector,
		log:  log,
		port: opts.Port,
	}, nil
}

// Collect 收集所有候选地址并按优先级降序排序。
func (c *Collector) Collect(ctx context.Context) ([]Candidate, error) {
	var out []Candidate

	hosts, err := c.collectHost()
	if err != nil {
		return nil, err
	}
	out = append(out, hosts...)

	if c.stun != nil {
		srflx := c.collectSrflx(ctx)
		out = append(out, srflx...)
	}

	// Relay 候选属 Phase 2c,此处留空。

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out, nil
}

// portOf 返回用于候选地址的端口。
//
// 优先使用显式 Port;否则从配置的第一个 listen 解析;再不行用默认端口。
func (c *Collector) portOf() uint16 {
	if c.port != 0 {
		return c.port
	}
	for _, l := range c.cfg.Listen {
		_, portStr, err := net.SplitHostPort(l.HostPort())
		if err != nil {
			continue
		}
		var p int
		if _, err := fmt.Sscanf(portStr, "%d", &p); err == nil && p > 0 && p <= 65535 {
			return uint16(p)
		}
	}
	return uint16(config.DefaultPort)
}

// collectHost 收集本机网卡地址作为 host 候选。
//
// 过滤规则:跳过 loopback、down、以及前缀匹配 candidate_skip_interfaces 的网卡。
func (c *Collector) collectHost() ([]Candidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("nat: list interfaces: %w", err)
	}

	port := c.portOf()
	skip := c.cfg.NAT.CandidateSkipInterfaces

	var out []Candidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if matchSkipPrefix(iface.Name, skip) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipNet.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if !ip.IsValid() || ip.IsLoopback() {
				continue
			}
			out = append(out, Candidate{
				Type:     CandidateHost,
				Addr:     netip.AddrPortFrom(ip, port),
				Priority: priority(CandidateHost, ip),
				Source:   iface.Name,
			})
		}
	}
	return out, nil
}

// collectSrflx 通过 STUN 收集反射候选。
func (c *Collector) collectSrflx(ctx context.Context) []Candidate {
	res, err := c.stun.Detect(ctx)
	if err != nil || res == nil || res.Mapped == nil {
		if err != nil {
			c.log.Debug("srflx collection failed", zap.Error(err))
		}
		return nil
	}
	m := res.Mapped
	return []Candidate{{
		Type:     CandidateSrflx,
		Addr:     m.AddrPort(),
		Priority: priority(CandidateSrflx, m.IP),
		Source:   m.Server,
	}}
}

// matchSkipPrefix 报告 name 是否以 skip 列表中某项为前缀。
func matchSkipPrefix(name string, skip []string) bool {
	for _, p := range skip {
		if p == "" {
			continue
		}
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
