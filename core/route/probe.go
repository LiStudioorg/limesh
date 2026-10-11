package route

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LiStudioorg/limesh/core/crypto"
)

// 探测相关常量(AGENTS.md 8.11)。
const (
	// ProbeInterval 是活跃链路的默认探测周期。
	ProbeInterval = 1 * time.Second
	// ProbeIdleInterval 是静默链路的默认探测周期(降频)。
	ProbeIdleInterval = 5 * time.Second
	// ProbePayloadSize 是探测包的目标载荷长度(含序号与时间戳)。
	ProbePayloadSize = 128
	// DefaultEWMAAlpha 是 RTT 平滑系数。
	DefaultEWMAAlpha = 0.2
	// ProbeSilenceThreshold 是判定链路静默的时长阈值。
	ProbeSilenceThreshold = 5 * time.Second
)

// 探测载荷字段偏移。
const (
	probeSeqOffset      = 0
	probeSendTimeOffset = 4
	probeEchoTimeOffset = 12
	probeRecvTimeOffset = 20
)

// 探测相关错误。
var (
	// ErrShortProbe 表示探测载荷长度不足。
	ErrShortProbe = errors.New("route: probe payload too short")
)

// LinkStats 是一条链路的实时质量统计。
type LinkStats struct {
	// RTT 是经 EWMA 平滑的往返延迟。
	RTT time.Duration
	// LossRate 是估算的丢包率 [0,1]。
	LossRate float64
	// LastSeen 是最近一次收到对端探测的时间。
	LastSeen time.Time
	// RTTMicros 是 RTT 的微秒表示,便于喂给策略。
	RTTMicros uint32
}

// ProbeSender 是 Prober 向对端发送探测载荷的抽象。
//
// 抽成接口使 Prober 不依赖 peer/transport,便于单测注入假发送器。
type ProbeSender interface {
	// SendProbe 向对端发送探测载荷。
	SendProbe(peerID [NodeIDSize]byte, payload []byte) error
	// SendProbeAck 向对端回送探测确认载荷。
	SendProbeAck(peerID [NodeIDSize]byte, payload []byte) error
}

// Prober 周期性地向所有直接邻居发送探测包并统计链路质量。
//
// 并发安全:Stats/HandleProbe/HandleProbeAck 可并发调用。
type Prober struct {
	sender ProbeSender
	// interval/idleInterval 是探测周期。
	interval     time.Duration
	idleInterval time.Duration
	// alpha 是 EWMA 平滑系数。
	alpha float64
	// now 可注入的时钟,便于测试。
	now func() time.Time

	mu    sync.RWMutex
	stats map[[NodeIDSize]byte]*linkState
	// seq 是探测序号。
	seq uint32
}

// linkState 是内部维护的单链路状态。
type linkState struct {
	rtt      float64 // 微秒
	rttInit  bool
	sent     uint64
	received uint64
	lastSeen time.Time
	// outstanding 记录已发送但未确认的探测,用于丢包率估算。
	outstanding uint64
}

// ProberOptions 是构造 Prober 的参数。
type ProberOptions struct {
	// Sender 是探测发送器,必填。
	Sender ProbeSender
	// Interval 覆盖默认探测周期。
	Interval time.Duration
	// IdleInterval 覆盖默认静默探测周期。
	IdleInterval time.Duration
	// EWMAAlpha 覆盖默认平滑系数。
	EWMAAlpha float64
	// Now 覆盖时钟(测试用途)。
	Now func() time.Time
}

// NewProber 构造一个 Prober。
func NewProber(opts ProberOptions) (*Prober, error) {
	if opts.Sender == nil {
		return nil, fmt.Errorf("route: new prober: sender must not be nil")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = ProbeInterval
	}
	idle := opts.IdleInterval
	if idle <= 0 {
		idle = ProbeIdleInterval
	}
	alpha := opts.EWMAAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = DefaultEWMAAlpha
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Prober{
		sender:       opts.Sender,
		interval:     interval,
		idleInterval: idle,
		alpha:        alpha,
		now:          now,
		stats:        make(map[[NodeIDSize]byte]*linkState),
	}, nil
}

// Register 注册一个需要探测的对端。
func (p *Prober) Register(peerID [NodeIDSize]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.stats[peerID]; !ok {
		p.stats[peerID] = &linkState{}
	}
}

// Unregister 移除一个对端的链路状态。
func (p *Prober) Unregister(peerID [NodeIDSize]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.stats, peerID)
}

// Peers 返回当前已注册的对端列表。
func (p *Prober) Peers() [][NodeIDSize]byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([][NodeIDSize]byte, 0, len(p.stats))
	for id := range p.stats {
		out = append(out, id)
	}
	return out
}

// Run 周期性地向所有已注册对端发送探测包,直到 ctx 结束。
func (p *Prober) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAll()
		}
	}
}

// probeAll 向所有注册对端发送一次探测。
//
// 静默链路(超过 ProbeSilenceThreshold 未收到回应)降低探测频率,
// 但**不停止**:仍需探测以发现链路恢复。
func (p *Prober) probeAll() {
	now := p.now()

	p.mu.Lock()
	defer p.mu.Unlock()

	for id, st := range p.stats {
		// 静默降频:仅在本 tick 恰好命中 idleInterval 的倍数时发送。
		if !st.lastSeen.IsZero() && now.Sub(st.lastSeen) > ProbeSilenceThreshold {
			if now.UnixNano()%int64(p.idleInterval) > int64(p.interval) {
				continue
			}
		}

		p.seq++
		payload := EncodeProbe(p.seq, now)
		st.sent++
		st.outstanding++
		// 发送失败不计入 received,自然反映为丢包。
		_ = p.sender.SendProbe(id, payload)
	}
}

// SendProbePayload 构造并返回一个探测载荷(供测试与上层直接使用)。
func (p *Prober) SendProbePayload(peerID [NodeIDSize]byte, seq uint32, now time.Time) []byte {
	return EncodeProbe(seq, now)
}

// Stats 返回指定对端的链路统计;未注册时返回零值。
func (p *Prober) Stats(peerID [NodeIDSize]byte) LinkStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	st, ok := p.stats[peerID]
	if !ok {
		return LinkStats{}
	}
	var rtt time.Duration
	if st.rttInit {
		rtt = time.Duration(uint64(st.rtt)) * time.Microsecond
	}
	return LinkStats{
		RTT:       rtt,
		LossRate:  lossRate(st),
		LastSeen:  st.lastSeen,
		RTTMicros: uint32(st.rtt),
	}
}

// HandleProbe 处理收到的探测包:向来源回送 ProbeAck。
//
// 回送时把原始 SendTime 一并带回,使发起方能独立计算 RTT。
func (p *Prober) HandleProbe(from [NodeIDSize]byte, payload []byte, recvAt time.Time) error {
	seq, sendNanos, err := DecodeProbe(payload)
	if err != nil {
		return fmt.Errorf("route: handle probe from %s: %w", crypto.NodeIDString(from), err)
	}
	ack := EncodeProbeAck(seq, sendNanos, recvAt)
	if err := p.sender.SendProbeAck(from, ack); err != nil {
		return fmt.Errorf("route: send probe ack to %s: %w", crypto.NodeIDString(from), err)
	}
	return nil
}

// HandleProbeAck 处理收到的探测确认,更新 EWMA 与丢包统计。
func (p *Prober) HandleProbeAck(from [NodeIDSize]byte, payload []byte, recvAt time.Time) error {
	_, sendNanos, err := DecodeProbeAck(payload)
	if err != nil {
		return fmt.Errorf("route: handle probe ack from %s: %w", crypto.NodeIDString(from), err)
	}

	rtt := recvAt.Sub(time.Unix(0, int64(sendNanos)))

	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.stats[from]
	if st == nil {
		st = &linkState{}
		p.stats[from] = st
	}

	sample := float64(rtt.Microseconds())
	if !st.rttInit {
		st.rtt = sample
		st.rttInit = true
	} else {
		st.rtt = p.alpha*sample + (1-p.alpha)*st.rtt
	}

	if st.outstanding > 0 {
		st.outstanding--
	}
	st.received++
	st.lastSeen = recvAt
	return nil
}

// lossRate 估算丢包率:未确认的探测比例。
func lossRate(st *linkState) float64 {
	if st.sent == 0 {
		return 0
	}
	unacked := st.outstanding
	if unacked == 0 {
		return 0
	}
	rate := float64(unacked) / float64(st.sent)
	if rate > 1 {
		rate = 1
	}
	return rate
}

// ---------------------------------------------------------------------------
// 线格式编解码
// ---------------------------------------------------------------------------

// EncodeProbe 把探测载荷编码为 TypeProbe 的 payload。
//
// 布局:ProbeSeq(4) + SendTime(8) + Padding(至 ProbePayloadSize)。
// 填充随机化载荷可降低被中间设备指纹识别的概率(AGENTS.md 8.6.5)。
func EncodeProbe(seq uint32, now time.Time) []byte {
	buf := make([]byte, ProbePayloadSize)
	binary.BigEndian.PutUint32(buf[probeSeqOffset:], seq)
	binary.BigEndian.PutUint64(buf[probeSendTimeOffset:], uint64(now.UnixNano()))
	// 其余字节留零;真实实现可填随机以抗指纹,Phase 1c 保持确定性便于测试。
	return buf
}

// DecodeProbe 解析探测载荷,返回 (seq, sendTimeNanos)。
func DecodeProbe(payload []byte) (uint32, uint64, error) {
	if len(payload) < probeSendTimeOffset+8 {
		return 0, 0, fmt.Errorf("route: decode probe: %d bytes: %w", len(payload), ErrShortProbe)
	}
	seq := binary.BigEndian.Uint32(payload[probeSeqOffset:])
	send := binary.BigEndian.Uint64(payload[probeSendTimeOffset:])
	return seq, send, nil
}

// EncodeProbeAck 把探测确认编码为 TypeProbeAck 的 payload。
//
// 布局:ProbeSeq(4) + EchoTime(8) + RecvTime(8)。
func EncodeProbeAck(seq uint32, echoNanos uint64, recvAt time.Time) []byte {
	buf := make([]byte, 4+8+8)
	binary.BigEndian.PutUint32(buf[0:], seq)
	binary.BigEndian.PutUint64(buf[4:], echoNanos)
	binary.BigEndian.PutUint64(buf[12:], uint64(recvAt.UnixNano()))
	return buf
}

// DecodeProbeAck 解析探测确认,返回 (seq, echoTimeNanos)。
func DecodeProbeAck(payload []byte) (uint32, uint64, error) {
	if len(payload) < 20 {
		return 0, 0, fmt.Errorf("route: decode probe ack: %d bytes: %w", len(payload), ErrShortProbe)
	}
	seq := binary.BigEndian.Uint32(payload[0:])
	echo := binary.BigEndian.Uint64(payload[4:])
	return seq, echo, nil
}
