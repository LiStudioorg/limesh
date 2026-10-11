package nat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// NATType 是 NAT 类型分类(RFC 3489 的简化子集)。
type NATType string

// NAT 类型取值。
//
// Phase 2a 只区分三级:open / cone / symmetric。精细的锥形细分
// (full/restricted/port-restricted)需要第三方服务器反向探测,属 Phase 2d。
const (
	// NATUnknown 表示尚未探测或探测失败。
	NATUnknown NATType = "unknown"
	// NATOpen 表示无 NAT(本机 IP 即公网映射)。
	NATOpen NATType = "open"
	// NATCone 表示锥形 NAT(不同 STUN 服务器观察到相同映射)。
	NATCone NATType = "cone"
	// NATSymmetric 表示对称 NAT(不同服务器观察到不同映射)。
	NATSymmetric NATType = "symmetric"
)

// String 返回类型名。
func (t NATType) String() string { return string(t) }

// 探测相关错误。
var (
	// ErrInsufficientServers 表示 STUN 服务器不足(至少需要 2 个)。
	ErrInsufficientServers = errors.New("nat: at least 2 STUN servers required for detection")
)

// DetectResult 是一次 NAT 探测的完整结果。
type DetectResult struct {
	// Type 是探测出的 NAT 类型。
	Type NATType
	// Mapped 是主映射地址(用于打洞);探测失败时为 nil。
	Mapped *MappedAddr
	// LocalIP 是发起探测的本地 UDP 地址(用于判断是否无 NAT)。
	LocalIP string
	// DetectedAt 是探测时间。
	DetectedAt time.Time
	// ServerCount 是成功响应的服务器数量。
	ServerCount int
}

// Detector 探测本端 NAT 类型。
//
// 它复用同一个 STUNClient(即同一个 UDP socket),因此所有查询共享本地端口,
// 映射差异确实反映 NAT 行为而非本地端口变化。
type Detector struct {
	client  *STUNClient
	servers []string
	// cacheTTL 是探测结果缓存时长;<=0 表示不缓存。
	cacheTTL time.Duration

	mu     sync.Mutex
	cached *DetectResult
}

// NewDetector 构造探测器。servers 至少需要 2 个才能区分锥形与对称。
func NewDetector(servers []string) (*Detector, error) {
	if len(servers) < 2 {
		return nil, fmt.Errorf("%w (got %d)", ErrInsufficientServers, len(servers))
	}
	client, err := NewSTUNClient(servers)
	if err != nil {
		return nil, err
	}
	return &Detector{
		client:   client,
		servers:  append([]string(nil), servers...),
		cacheTTL: DefaultNATCacheTTL,
	}, nil
}

// DefaultNATCacheTTL 是默认的探测结果缓存时长。
const DefaultNATCacheTTL = 5 * time.Minute

// SetTimeout 覆盖 STUN 查询超时。
func (d *Detector) SetTimeout(t time.Duration) { d.client.SetTimeout(t) }

// SetCacheTTL 覆盖探测结果缓存时长;<=0 表示禁用缓存。
func (d *Detector) SetCacheTTL(t time.Duration) { d.cacheTTL = t }

// Close 释放底层 socket。
func (d *Detector) Close() error { return d.client.Close() }

// Cached 返回缓存中的探测结果;无缓存或已过期时返回 nil。
func (d *Detector) Cached() *DetectResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cached == nil {
		return nil
	}
	if d.cacheTTL > 0 && time.Since(d.cached.DetectedAt) > d.cacheTTL {
		return nil
	}
	return d.cached
}

// Detect 执行一次 NAT 类型探测。
//
// 流程:
//  1. 并发/顺序查询多个 STUN 服务器,取前两个成功结果;
//  2. 若映射 IP+端口一致 → 锥形;不一致 → 对称;
//  3. 若映射 IP 与本机本地 IP 相同(且无 NAT)→ open。
//
// 返回值:
//   - Type:探测结果;探测失败时为 NATUnknown;
//   - Mapped:主映射(第一个成功结果),用于打洞;
//   - error:至少一个服务器成功即返回 nil;全部失败才返回错误。
//
// 探测失败**不阻塞节点运行**:调用方应把 NATUnknown 视为"暂不可用"而非致命错误。
func (d *Detector) Detect(ctx context.Context) (*DetectResult, error) {
	// 复用缓存的映射一致性判定,但每次都重新查询以确保新鲜。
	results, err := d.queryAll(ctx)
	if err != nil {
		return &DetectResult{Type: NATUnknown, DetectedAt: time.Now()},
			fmt.Errorf("nat: detect: %w", err)
	}

	primary := results[0].mapped

	res := &DetectResult{
		Mapped:      primary,
		DetectedAt:  time.Now(),
		ServerCount: len(results),
	}

	if localIP := d.client.LocalIP(); localIP != "" {
		res.LocalIP = localIP
	}

	// 判定:所有成功响应的映射是否一致。
	consistent := true
	for _, r := range results[1:] {
		if r.mapped.IP != primary.IP || r.mapped.Port != primary.Port {
			consistent = false
			break
		}
	}

	switch {
	case !consistent:
		res.Type = NATSymmetric
	case d.isOpen(primary):
		res.Type = NATOpen
	default:
		res.Type = NATCone
	}

	if d.cacheTTL > 0 {
		d.mu.Lock()
		d.cached = res
		d.mu.Unlock()
	}

	return res, nil
}

// isOpen 判断映射是否等于本机地址(无 NAT)。
func (d *Detector) isOpen(m *MappedAddr) bool {
	local := d.client.LocalIP()
	if local == "" {
		return false
	}
	return m.IP.String() == local
}

// serverResult 是单服务器的查询结果。
type serverResult struct {
	server string
	mapped *MappedAddr
}

// queryAll 并发查询所有服务器,返回成功结果(按返回顺序)。
//
// 至少一个成功即视为成功;全部失败返回错误。
func (d *Detector) queryAll(ctx context.Context) ([]serverResult, error) {
	if err := d.client.ensureConn(); err != nil {
		return nil, err
	}

	type res struct {
		r   serverResult
		err error
	}
	ch := make(chan res, len(d.servers))
	for _, s := range d.servers {
		go func(server string) {
			m, err := d.client.Query(ctx, server)
			ch <- res{r: serverResult{server: server, mapped: m}, err: err}
		}(s)
	}

	var (
		out     []serverResult
		lastErr error
	)
	for i := 0; i < len(d.servers); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			if r.err != nil {
				lastErr = r.err
				continue
			}
			out = append(out, r.r)
		}
	}

	if len(out) == 0 {
		if lastErr == nil {
			lastErr = ErrNoResponse
		}
		return nil, lastErr
	}
	return out, nil
}
