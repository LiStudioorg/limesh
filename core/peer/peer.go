// Package peer 实现 Limesh 的 Peer 状态机与全量对端注册表。
//
// 状态机定义见 AGENTS.md 8.9.1。本包只负责**状态推进与审计**,不直接收发
// 网络包:所有外部动作(发起握手、打洞、保活、中继)通过注入的驱动接口触发,
// 使状态机可以在无网络的环境下被完整测试。
//
// 命名以 AGENTS.md 为准(§2:命名是项目内契约)。任务描述中的
// Idle/Handshaking/Probing/Direct/Relay 在此对应为
// Idle/Handshake/Probing/Direct/Relayed;其中 Probe 表示打洞/探测中。
package peer

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/nat"
	"github.com/LiStudioorg/limesh/core/session"
)

// 协议常量,取值见 AGENTS.md 8.11。
const (
	// KeepaliveInterval 是保活周期。
	KeepaliveInterval = 20 * time.Second
	// PeerDegradedAfter 是连续无响应达到该时长后转入 Degraded 的判定阈值。
	PeerDegradedAfter = 60 * time.Second
	// PeerDeadAfter 是连续无响应达到该时长后转入 Dead 的判定阈值。
	PeerDeadAfter = 120 * time.Second
	// PunchTimeout 是打洞总超时。
	PunchTimeout = 5 * time.Second
	// RepunchInterval 是 Relayed 状态下重新尝试打洞的周期。
	RepunchInterval = 60 * time.Second
	// EWMAAlpha 是延迟平滑系数。
	EWMAAlpha = 0.2
)

// State 是 Peer 的状态。
//
// 取值与 AGENTS.md 8.9.1 的状态图一一对应。新增状态时须同步修订该文档。
type State int

const (
	// StateIdle 表示已发现对端但尚未发起连接。
	StateIdle State = iota
	// StateHandshake 表示正在执行 Noise IK 握手。
	StateHandshake
	// StateProbing 表示握手完成,正在打洞/探测直连路径。
	StateProbing
	// StateDirect 表示已建立直连路径。
	StateDirect
	// StateRelayed 表示经中继转发,后台仍在周期性尝试重新打洞。
	StateRelayed
	// StateDegraded 表示连续一个降级周期无响应,仍可用但不再作为首选路径。
	StateDegraded
	// StateDead 表示连续多个周期无响应,应从路由表移除。
	StateDead
	// StateFailed 表示握手重试耗尽,无法建立会话。
	StateFailed
)

// String 返回状态的中文名称,用于日志与 CLI 输出。
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateHandshake:
		return "handshake"
	case StateProbing:
		return "probing"
	case StateDirect:
		return "direct"
	case StateRelayed:
		return "relayed"
	case StateDegraded:
		return "degraded"
	case StateDead:
		return "dead"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// 状态机错误。
var (
	// ErrInvalidTransition 表示请求的状态转换不合法。
	ErrInvalidTransition = errors.New("peer: invalid state transition")
	// ErrClosed 表示 Peer 或 Manager 已关闭。
	ErrClosed = errors.New("peer: closed")
	// ErrNilPeer 表示传入的 Peer 为空。
	ErrNilPeer = errors.New("peer: nil peer")
)

// Transition 记录一次状态转换,用于审计。
//
// 状态机要求每次转换都是**可审计**的:旧状态、新状态与原因三者齐备,
// 使线上排错时可以还原一条 Peer 的完整生命周期。
type Transition struct {
	// From 是转换前的状态。
	From State
	// To 是转换后的状态。
	To State
	// Reason 是人类可读的转换原因。
	Reason string
	// At 是转换发生的时间。
	At time.Time
}

// String 返回便于日志展示的一行摘要。
func (t Transition) String() string {
	return fmt.Sprintf("%s -> %s (%s)", t.From, t.To, t.Reason)
}

// Clock 是可注入的时钟,便于测试超时逻辑而不必真的等待。
//
// 生产代码使用 SystemClock;测试可注入手工推进的假时钟。
type Clock interface {
	// Now 返回当前时间。
	Now() time.Time
}

// SystemClock 是基于 time.Now 的默认时钟。
type SystemClock struct{}

// Now 返回当前时间。
func (SystemClock) Now() time.Time { return time.Now() }

// Driver 是状态机与外部世界交互的接口。
//
// 状态机本身不碰网络:握手、打洞、保活、中继都由 Driver 实现。这样做的收益
// 是状态机可以完全脱离网络被测试,且数据面实现替换时状态机无需改动。
type Driver interface {
	// StartHandshake 请求对指定对端发起 Noise IK 握手。
	StartHandshake(peerID [crypto.NodeIDSize]byte) error
	// StartPunch 请求对指定对端发起打洞。Phase 1a 允许为空实现。
	StartPunch(peerID [crypto.NodeIDSize]byte) error
	// SendKeepalive 向对端发送一个保活包。
	SendKeepalive(peerID [crypto.NodeIDSize]byte) error
}

// Options 是构造 Peer 的参数。
type Options struct {
	// NodeID 是对端身份,必填。
	NodeID [crypto.NodeIDSize]byte
	// Addr 是对端地址(host:port),可为空(由控制面/发现机制填充)。
	Addr string
	// Clock 是注入的时钟;为空时使用 SystemClock。
	Clock Clock
	// Driver 是外部动作驱动;可为 nil(纯状态机模式)。
	Driver Driver
	// Session 是握手建立的会话;可为 nil,由 SetSession 补充。
	Session *session.Session
	// State 是初始状态;零值为 StateIdle。
	State State
}

// Peer 是单个对端的状态机。
//
// 并发安全:所有导出方法都可由多个 goroutine 并发调用。
type Peer struct {
	// NodeID 是对端身份。构造后不可变,因此读取无需加锁。
	NodeID [crypto.NodeIDSize]byte

	mu       sync.Mutex
	addr     string
	state    State
	clock    Clock
	driver   Driver
	session  *session.Session
	lastSeen time.Time
	// lastPunch 记录最近一次发起打洞的时间,用于 RepunchInterval 节流。
	lastPunch time.Time
	// rttEWMA 是平滑后的往返延迟,零值表示尚无样本。
	rttEWMA float64
	// rttInit 标记是否已有 EWMA 样本。
	rttInit bool
	// handshakeRetries 记录握手已尝试次数。
	handshakeRetries int
	// history 记录全部状态转换,仅在开启审计时保留。
	history []Transition
	closed  bool

	// remoteCandidates 是对端交换来的候选地址(可能来自握手或后续更新)。
	remoteCandidates atomic.Pointer[[]nat.Candidate]
	// localCandidates 是本端为该对端准备的候选地址。
	localCandidates atomic.Pointer[[]nat.Candidate]
}

// New 按参数构造一个 Peer。
func New(opts Options) (*Peer, error) {
	clock := opts.Clock
	if clock == nil {
		clock = SystemClock{}
	}

	p := &Peer{
		NodeID:   opts.NodeID,
		addr:     opts.Addr,
		state:    opts.State,
		clock:    clock,
		driver:   opts.Driver,
		session:  opts.Session,
		lastSeen: clock.Now(),
	}
	return p, nil
}

// NodeIDHex 返回对端 NodeID 的十六进制表示。
func (p *Peer) NodeIDHex() string {
	return crypto.NodeIDString(p.NodeID)
}

// VirtualIP 返回对端由 NodeID 派生的虚拟 IPv4 地址。
func (p *Peer) VirtualIP() string {
	return crypto.VirtualIPString(p.NodeID, true)
}

// State 返回当前状态。
func (p *Peer) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Addr 返回对端地址。
func (p *Peer) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

// SetAddr 更新对端地址。
func (p *Peer) SetAddr(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addr = addr
}

// Session 返回当前会话,未建立时返回 nil。
func (p *Peer) Session() *session.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session
}

// SetSession 设置(或替换)会话。
func (p *Peer) SetSession(s *session.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
}

// Driver 返回注入的动作驱动。
func (p *Peer) Driver() Driver {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.driver
}

// SetRemoteCandidates 保存对端交换来的候选地址。
//
// 存储的是副本,调用方可安全复用传入切片。Phase 2a 仅存储;打洞(2b)会消费它。
func (p *Peer) SetRemoteCandidates(cands []nat.Candidate) {
	cp := append([]nat.Candidate(nil), cands...)
	p.remoteCandidates.Store(&cp)
}

// RemoteCandidates 返回对端候选地址的副本。
func (p *Peer) RemoteCandidates() []nat.Candidate {
	ptr := p.remoteCandidates.Load()
	if ptr == nil {
		return nil
	}
	return append([]nat.Candidate(nil), (*ptr)...)
}

// SetLocalCandidates 保存本端为该对端准备的候选地址。
func (p *Peer) SetLocalCandidates(cands []nat.Candidate) {
	cp := append([]nat.Candidate(nil), cands...)
	p.localCandidates.Store(&cp)
}

// LocalCandidates 返回本端候选地址的副本。
func (p *Peer) LocalCandidates() []nat.Candidate {
	ptr := p.localCandidates.Load()
	if ptr == nil {
		return nil
	}
	return append([]nat.Candidate(nil), (*ptr)...)
}

// LastSeen 返回最近一次收到对端包的时间。
func (p *Peer) LastSeen() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSeen
}

// Idle 返回自最近一次收到对端包以来的时长。
//
// 以注入时钟为基准,使超时测试无需真实等待。
func (p *Peer) Idle() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clock.Now().Sub(p.lastSeen)
}

// RTT 返回平滑后的往返延迟估计;ok 为 false 表示尚无样本。
func (p *Peer) RTT() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.rttInit {
		return 0, false
	}
	return time.Duration(p.rttEWMA), true
}

// History 返回状态转换历史的快照。
func (p *Peer) History() []Transition {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Transition(nil), p.history...)
}

// Touch 记录一次收到对端包,刷新存活时间。
//
// 同时用于保活样本:收到任何来自对端的包都意味着链路仍然活着。
func (p *Peer) Touch() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastSeen = p.clock.Now()
}

// ObserveRTT 以一次往返延迟样本更新 EWMA。
//
// EWMA 平滑是**必需**而非优化(AGENTS.md 8.8.1):未平滑的延迟抖动会让
// TCP 流在两条路径间反复迁移进而乱序。
func (p *Peer) ObserveRTT(sample time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := float64(sample)
	if !p.rttInit {
		p.rttEWMA = s
		p.rttInit = true
		return
	}
	p.rttEWMA = EWMAAlpha*s + (1-EWMAAlpha)*p.rttEWMA
}

// transfer 在持有锁的前提下执行一次状态转换。
//
// 返回是否真的发生了状态变化。非法转换返回 ErrInvalidTransition。
// reason 会写入审计历史。
func (p *Peer) transfer(to State, reason string) error {
	if p.closed {
		return fmt.Errorf("peer %s: transition to %s: %w", p.NodeIDHex(), to, ErrClosed)
	}
	if !canTransition(p.state, to) {
		return fmt.Errorf("peer %s: %s -> %s: %w", p.NodeIDHex(), p.state, to, ErrInvalidTransition)
	}
	if p.state == to {
		// 同态转换视为无操作,不记录历史,避免审计噪音。
		return nil
	}

	from := p.state
	p.state = to
	p.history = append(p.history, Transition{
		From:   from,
		To:     to,
		Reason: reason,
		At:     p.clock.Now(),
	})
	return nil
}

// transition 是 transfer 的加锁封装,供导出方法使用。
func (p *Peer) transition(to State, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.transfer(to, reason)
}

// canTransition 判断从 from 到 to 的状态转换是否合法。
//
// 合法转换集合与 AGENTS.md 8.9.1 的状态图严格对应:
//
//	Idle → Handshake → Probing → Direct ⇄ Relayed
//	idle/handshake 失败 → Failed
//	direct/relayed/... 无响应 → Degraded → Dead
//
// 允许的"重入"转换(如 Direct → Direct)由 transfer 视为无操作。
func canTransition(from, to State) bool {
	if from == to {
		return true
	}

	switch from {
	case StateIdle:
		// 发现对端后发起握手;也可直接判定失败(如对端不可达)。
		return to == StateHandshake || to == StateFailed
	case StateHandshake:
		// 握手成功进入打洞;失败进入 Failed;对端消失可直接判死。
		return to == StateProbing || to == StateFailed || to == StateDead
	case StateProbing:
		// 打洞成功直连;超时降级中继;重试耗尽判失败。
		return to == StateDirect || to == StateRelayed || to == StateFailed
	case StateDirect:
		// 直连失效转中继;无响应降级/死亡。
		return to == StateRelayed || to == StateDegraded || to == StateDead
	case StateRelayed:
		// 重新打洞先进入 Probing;无响应降级/死亡。
		return to == StateProbing || to == StateDegraded || to == StateDead
	case StateDegraded:
		// 恢复直连/中继,或继续恶化到 Dead。
		return to == StateDirect || to == StateRelayed || to == StateDead
	case StateDead:
		// 允许从死亡状态复活(对端重新出现)。
		return to == StateHandshake
	case StateFailed:
		// 允许重试握手。
		return to == StateHandshake
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 对外状态推进入口
//
// 这些方法封装了合法的转换路径,上层(数据面)只需在对应事件发生时调用,
// 无需自己判断当前状态。每个方法都是并发安全的。
// ---------------------------------------------------------------------------

// StartHandshake 从 Idle/Degraded/Dead/Failed 发起一次握手。
//
// 若已注入 Driver,会同时请求其发起 Noise 握手;Driver 错误不改变状态,
// 使调用方可以安全重试。
func (p *Peer) StartHandshake() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("peer %s: start handshake: %w", p.NodeIDHex(), ErrClosed)
	}
	if err := p.transfer(StateHandshake, "start handshake"); err != nil {
		p.mu.Unlock()
		return err
	}
	p.handshakeRetries++
	driver := p.driver
	id := p.NodeID
	p.mu.Unlock()

	if driver != nil {
		if err := driver.StartHandshake(id); err != nil {
			return fmt.Errorf("peer %s: driver start handshake: %w", p.NodeIDHex(), err)
		}
	}
	return nil
}

// OnHandshakeSuccess 处理握手成功事件:进入打洞阶段。
//
// s 为握手派生出的会话,可以是 nil(由 SetSession 后续补充)。
func (p *Peer) OnHandshakeSuccess(s *session.Session) error {
	p.mu.Lock()
	if s != nil {
		p.session = s
	}
	p.handshakeRetries = 0
	p.lastSeen = p.clock.Now()
	err := p.transfer(StateProbing, "handshake ok")
	driver := p.driver
	id := p.NodeID
	p.mu.Unlock()

	if err != nil {
		return err
	}
	if driver != nil {
		if derr := driver.StartPunch(id); derr != nil {
			return fmt.Errorf("peer %s: driver start punch: %w", p.NodeIDHex(), derr)
		}
	}
	return nil
}

// OnHandshakeFailure 处理握手失败:重试耗尽时转入 Failed。
//
// 返回耗尽状态,供上层决定是否回退到中继协商。
func (p *Peer) OnHandshakeFailure(reason string) (exhausted bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.handshakeRetries < maxHandshakeRetries(p.handshakeRetries) {
		// 尚未耗尽:保持 Handshake 状态等待重试。
		return false, nil
	}
	if err := p.transfer(StateFailed, reason); err != nil {
		return false, err
	}
	return true, nil
}

// OnPunchSuccess 处理打洞成功:进入直连。
func (p *Peer) OnPunchSuccess() error {
	return p.transition(StateDirect, "punch ok")
}

// OnPunchTimeout 处理打洞超时:降级到中继。
func (p *Peer) OnPunchTimeout() error {
	return p.transition(StateRelayed, "punch timeout, fallback to relay")
}

// OnKeepalive 处理收到一个来自对端的包(保活或数据)。
//
// 刷新存活时间;若处在 Degraded 状态则恢复到原路径。返回恢复后的状态,
// 未发生恢复时返回当前状态。
func (p *Peer) OnKeepalive() State {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lastSeen = p.clock.Now()

	// 从 Degraded 恢复到中继(最保守的路径):直连是否可用由上层打洞决定。
	if p.state == StateDegraded {
		if err := p.transfer(StateRelayed, "keepalive received, recovered"); err != nil {
			// 恢复失败不应中断数据面,保留原状态。
			return p.state
		}
	}
	return p.state
}

// ShouldRepunch 报告 Relayed 状态下是否已到达重新打洞的时间。
func (p *Peer) ShouldRepunch() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != StateRelayed {
		return false
	}
	if p.lastPunch.IsZero() {
		return true
	}
	return p.clock.Now().Sub(p.lastPunch) >= RepunchInterval
}

// Repunch 在 Relayed 状态下发起一次重新打洞,进入 Probing。
func (p *Peer) Repunch() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("peer %s: repunch: %w", p.NodeIDHex(), ErrClosed)
	}
	p.lastPunch = p.clock.Now()
	if err := p.transfer(StateProbing, "repunch"); err != nil {
		p.mu.Unlock()
		return err
	}
	driver := p.driver
	id := p.NodeID
	p.mu.Unlock()

	if driver != nil {
		if err := driver.StartPunch(id); err != nil {
			return fmt.Errorf("peer %s: driver repunch: %w", p.NodeIDHex(), err)
		}
	}
	return nil
}

// OnKeepaliveDue 是保活定时器触发时的入口:向对端发送保活包。
func (p *Peer) OnKeepaliveDue() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("peer %s: keepalive: %w", p.NodeIDHex(), ErrClosed)
	}
	driver := p.driver
	id := p.NodeID
	p.mu.Unlock()

	if driver == nil {
		return nil
	}
	if err := driver.SendKeepalive(id); err != nil {
		return fmt.Errorf("peer %s: driver keepalive: %w", p.NodeIDHex(), err)
	}
	return nil
}

// Tick 依据空闲时长推进超时相关状态。
//
// 规则(AGENTS.md 8.9.1):
//   - 连续 3 个保活周期(60s)无响应 → Degraded
//   - 连续 6 个保活周期(120s)无响应 → Dead
//
// 对已处于 Dead 的 Peer 不做任何事。返回是否发生了状态变化。
func (p *Peer) Tick() (changed bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || p.state == StateDead {
		return false, nil
	}
	// 尚未建立会话的中间态不参与超时判定:它们由握手/打洞超时逻辑处理。
	switch p.state {
	case StateIdle, StateHandshake, StateProbing, StateFailed:
		return false, nil
	}

	idle := p.clock.Now().Sub(p.lastSeen)

	if idle >= PeerDeadAfter {
		if err := p.transfer(StateDead, "no keepalive within dead threshold"); err != nil {
			return false, err
		}
		return true, nil
	}
	if idle >= PeerDegradedAfter && p.state != StateDegraded {
		if err := p.transfer(StateDegraded, "no keepalive within degraded threshold"); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// Close 关闭 Peer,释放会话。重复调用是安全的。
func (p *Peer) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	s := p.session
	p.session = nil
	p.mu.Unlock()

	if s != nil {
		s.Close()
	}
	return nil
}

// Closed 报告 Peer 是否已关闭。
func (p *Peer) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// maxHandshakeRetries 返回允许的最大握手尝试次数。
//
// 抽成函数便于在后续阶段改为可配置,同时避免在 Transfer 中引入对
// handshake 包的依赖(那会造成 peer → handshake → session 的循环)。
func maxHandshakeRetries(int) int { return 5 }
