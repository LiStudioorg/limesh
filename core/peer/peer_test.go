package peer

import (
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/nat"
)

// fakeClock 是手工推进的假时钟,使超时测试无需真实等待。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock 从固定起点创建假时钟。
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

// Now 返回当前(可能被推进过的)时间。
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进时钟。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeDriver 记录状态机请求的外部动作。
type fakeDriver struct {
	mu          sync.Mutex
	handshakes  int
	punches     int
	keepalives  int
	failHandshk bool
}

// StartHandshake 记录一次握手请求。
func (d *fakeDriver) StartHandshake(id [crypto.NodeIDSize]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handshakes++
	if d.failHandshk {
		return errors.New("driver: handshake unavailable")
	}
	return nil
}

// StartPunch 记录一次打洞请求。
func (d *fakeDriver) StartPunch(id [crypto.NodeIDSize]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.punches++
	return nil
}

// SendKeepalive 记录一次保活发送。
func (d *fakeDriver) SendKeepalive(id [crypto.NodeIDSize]byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keepalives++
	return nil
}

// counts 返回累计动作计数。
func (d *fakeDriver) counts() (handshake, punch, keepalive int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handshakes, d.punches, d.keepalives
}

// testNodeID 构造一个确定性的 NodeID。
func testNodeID(b byte) [crypto.NodeIDSize]byte {
	var id [crypto.NodeIDSize]byte
	for i := range id {
		id[i] = b + byte(i)
	}
	return id
}

// newTestPeer 构造一个带假时钟与假驱动的 Peer。
func newTestPeer(t *testing.T, b byte) (*Peer, *fakeClock, *fakeDriver) {
	t.Helper()
	clock := newFakeClock()
	drv := &fakeDriver{}
	p, err := New(Options{
		NodeID: testNodeID(b),
		Addr:   "127.0.0.1:11010",
		Clock:  clock,
		Driver: drv,
	})
	require.NoError(t, err)
	return p, clock, drv
}

// TestStateString 覆盖所有状态名称。
func TestStateString(t *testing.T) {
	assert.Equal(t, "idle", StateIdle.String())
	assert.Equal(t, "handshake", StateHandshake.String())
	assert.Equal(t, "probing", StateProbing.String())
	assert.Equal(t, "direct", StateDirect.String())
	assert.Equal(t, "relayed", StateRelayed.String())
	assert.Equal(t, "degraded", StateDegraded.String())
	assert.Equal(t, "dead", StateDead.String())
	assert.Equal(t, "failed", StateFailed.String())
	assert.Equal(t, "unknown(99)", State(99).String())
}

// TestCanTransition 覆盖全部合法与非法转换。
func TestCanTransition(t *testing.T) {
	tests := []struct {
		from, to State
		want     bool
	}{
		// 同态一律允许(由 transfer 视为无操作)。
		{StateIdle, StateIdle, true},
		{StateDirect, StateDirect, true},

		// 正常路径。
		{StateIdle, StateHandshake, true},
		{StateHandshake, StateProbing, true},
		{StateProbing, StateDirect, true},
		{StateProbing, StateRelayed, true},
		{StateDirect, StateRelayed, true},
		{StateRelayed, StateProbing, true},

		// 失败路径。
		{StateIdle, StateFailed, true},
		{StateHandshake, StateFailed, true},
		{StateProbing, StateFailed, true},
		{StateHandshake, StateDead, true},

		// 降级与死亡。
		{StateDirect, StateDegraded, true},
		{StateRelayed, StateDegraded, true},
		{StateDegraded, StateDirect, true},
		{StateDegraded, StateRelayed, true},
		{StateDegraded, StateDead, true},
		{StateDirect, StateDead, true},
		{StateRelayed, StateDead, true},

		// 复活与重试。
		{StateDead, StateHandshake, true},
		{StateFailed, StateHandshake, true},

		// 非法转换。
		{StateIdle, StateDirect, false},
		{StateIdle, StateRelayed, false},
		{StateHandshake, StateDirect, false},
		{StateProbing, StateHandshake, false},
		{StateDead, StateDirect, false},
		{StateFailed, StateDirect, false},
		{StateDegraded, StateHandshake, false},
	}

	for _, tt := range tests {
		t.Run(tt.from.String()+"->"+tt.to.String(), func(t *testing.T) {
			assert.Equal(t, tt.want, canTransition(tt.from, tt.to))
		})
	}
}

// TestFullHappyPath 覆盖 Idle→Handshake→Probing→Direct 的完整成功路径。
func TestFullHappyPath(t *testing.T) {
	p, _, drv := newTestPeer(t, 1)

	require.NoError(t, p.StartHandshake())
	assert.Equal(t, StateHandshake, p.State())

	require.NoError(t, p.OnHandshakeSuccess(nil))
	assert.Equal(t, StateProbing, p.State())

	require.NoError(t, p.OnPunchSuccess())
	assert.Equal(t, StateDirect, p.State())

	hs, punch, _ := drv.counts()
	assert.Equal(t, 1, hs, "应请求一次握手")
	assert.Equal(t, 1, punch, "应请求一次打洞")

	// 审计历史应记录每一次转换。
	hist := p.History()
	require.Len(t, hist, 3)
	assert.Equal(t, StateIdle, hist[0].From)
	assert.Equal(t, StateHandshake, hist[0].To)
	assert.Equal(t, StateProbing, hist[2].From)
	assert.Equal(t, StateDirect, hist[2].To)
	assert.NotEmpty(t, hist[0].Reason, "每次转换必须带原因")
}

// TestPunchTimeoutFallsBackToRelay 验证打洞超时降级到中继。
func TestPunchTimeoutFallsBackToRelay(t *testing.T) {
	p, _, _ := newTestPeer(t, 2)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchTimeout())

	assert.Equal(t, StateRelayed, p.State())
}

// TestInvalidTransitionRejected 验证非法转换被拒绝且不改变状态。
func TestInvalidTransitionRejected(t *testing.T) {
	p, _, _ := newTestPeer(t, 3)

	// Idle 直接到 Direct 非法。
	err := p.transition(StateDirect, "bogus")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
	assert.Equal(t, StateIdle, p.State())
	assert.Empty(t, p.History(), "失败的转换不应写入历史")
}

// TestHandshakeFailureExhaustsRetries 验证握手重试耗尽后转入 Failed。
func TestHandshakeFailureExhaustsRetries(t *testing.T) {
	p, _, _ := newTestPeer(t, 4)

	// 前若干次重试不耗尽。
	for i := 0; i < 4; i++ {
		require.NoError(t, p.StartHandshake())
		exhausted, err := p.OnHandshakeFailure("noise failed")
		require.NoError(t, err)
		assert.False(t, exhausted, "第 %d 次不应耗尽", i+1)
		assert.Equal(t, StateHandshake, p.State())
	}

	// 第 5 次达到上限。
	require.NoError(t, p.StartHandshake())
	exhausted, err := p.OnHandshakeFailure("noise failed")
	require.NoError(t, err)
	assert.True(t, exhausted)
	assert.Equal(t, StateFailed, p.State())
}

// TestDegradedThenDead 使用可注入时钟验证超时驱动的 Degraded → Dead 推进。
func TestDegradedThenDead(t *testing.T) {
	p, clock, _ := newTestPeer(t, 5)

	// 进入 Direct。
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	// 未到降级阈值:不变。
	clock.Advance(PeerDegradedAfter - time.Second)
	changed, err := p.Tick()
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, StateDirect, p.State())

	// 到达降级阈值。
	clock.Advance(time.Second)
	changed, err = p.Tick()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, StateDegraded, p.State())

	// 到达死亡阈值。
	clock.Advance(PeerDeadAfter - PeerDegradedAfter)
	changed, err = p.Tick()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, StateDead, p.State())

	// 已死亡后 Tick 不再变化。
	changed, err = p.Tick()
	require.NoError(t, err)
	assert.False(t, changed)
}

// TestKeepaliveRecoversFromDegraded 验证收到保活后从 Degraded 恢复。
func TestKeepaliveRecoversFromDegraded(t *testing.T) {
	p, clock, _ := newTestPeer(t, 6)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	clock.Advance(PeerDegradedAfter)
	changed, err := p.Tick()
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, StateDegraded, p.State())

	// 收到保活应刷新存活时间并恢复。
	got := p.OnKeepalive()
	assert.Equal(t, StateRelayed, got)
	assert.Equal(t, time.Duration(0), p.Idle(), "保活应刷新 lastSeen")
}

// TestKeepaliveOnDeadDoesNotResurrect 验证 Dead 状态下保活不会自行复活
// (复活必须显式重新握手)。
func TestKeepaliveOnDeadDoesNotResurrect(t *testing.T) {
	p, clock, _ := newTestPeer(t, 7)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	clock.Advance(PeerDeadAfter)
	changed, err := p.Tick()
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, StateDead, p.State())

	got := p.OnKeepalive()
	assert.Equal(t, StateDead, got, "保活不应让 Dead 自行复活")
}

// TestRepunchThrottle 验证 Relayed 状态下重新打洞受 RepunchInterval 节流。
func TestRepunchThrottle(t *testing.T) {
	p, clock, _ := newTestPeer(t, 8)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchTimeout())
	require.Equal(t, StateRelayed, p.State())

	// 刚进入 Relayed:尚无打洞记录,允许立即重打。
	assert.True(t, p.ShouldRepunch())

	require.NoError(t, p.Repunch())
	assert.Equal(t, StateProbing, p.State())

	// 回退到 Relayed 再检查节流。
	require.NoError(t, p.OnPunchTimeout())
	assert.False(t, p.ShouldRepunch(), "未到间隔不应重打")

	clock.Advance(RepunchInterval)
	assert.True(t, p.ShouldRepunch(), "到达间隔后应允许重打")
}

// TestShouldRepunchOnlyInRelayed 验证非中继状态不触发重打。
func TestShouldRepunchOnlyInRelayed(t *testing.T) {
	p, _, _ := newTestPeer(t, 9)
	assert.False(t, p.ShouldRepunch(), "Idle 状态不应触发重打")

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())
	assert.False(t, p.ShouldRepunch(), "Direct 状态不应触发重打")
}

// TestObserveRTTEWMA 验证延迟 EWMA 平滑。
func TestObserveRTTEWMA(t *testing.T) {
	p, _, _ := newTestPeer(t, 10)

	_, ok := p.RTT()
	assert.False(t, ok, "无样本时 ok 应为 false")

	p.ObserveRTT(100 * time.Millisecond)
	got, ok := p.RTT()
	require.True(t, ok)
	assert.Equal(t, 100*time.Millisecond, got)

	// 第二次样本按 EWMAAlpha 平滑:0.2*200 + 0.8*100 = 120。
	p.ObserveRTT(200 * time.Millisecond)
	got, ok = p.RTT()
	require.True(t, ok)
	assert.Equal(t, 120*time.Millisecond, got)
}

// TestIdentityDerivations 验证 NodeID 十六进制与虚拟 IP 派生。
func TestIdentityDerivations(t *testing.T) {
	p, _, _ := newTestPeer(t, 11)

	assert.Equal(t, crypto.NodeIDString(p.NodeID), p.NodeIDHex())
	assert.Equal(t, crypto.VirtualIPString(p.NodeID, true), p.VirtualIP())
	assert.NotEmpty(t, p.VirtualIP())
}

// TestAccessors 覆盖地址、会话与驱动访问器。
func TestAccessors(t *testing.T) {
	p, _, drv := newTestPeer(t, 12)

	assert.Equal(t, "127.0.0.1:11010", p.Addr())
	p.SetAddr("10.0.0.1:11010")
	assert.Equal(t, "10.0.0.1:11010", p.Addr())

	assert.Nil(t, p.Session())
	assert.Same(t, drv, p.Driver())

	require.False(t, p.Closed())
	require.NoError(t, p.Close())
	assert.True(t, p.Closed())
	// 重复关闭安全。
	require.NoError(t, p.Close())
}

// TestStartHandshakeAfterClose 验证关闭后拒绝推进。
func TestStartHandshakeAfterClose(t *testing.T) {
	p, _, _ := newTestPeer(t, 13)
	require.NoError(t, p.Close())

	err := p.StartHandshake()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClosed)
}

// TestDriverErrorPropagated 验证驱动错误被包装返回且不静默吞掉。
func TestDriverErrorPropagated(t *testing.T) {
	clock := newFakeClock()
	drv := &fakeDriver{failHandshk: true}
	p, err := New(Options{
		NodeID: testNodeID(14),
		Clock:  clock,
		Driver: drv,
	})
	require.NoError(t, err)

	err = p.StartHandshake()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "handshake unavailable")
	// 状态已推进到 Handshake —— 驱动失败不回滚状态机,由上层重试。
	assert.Equal(t, StateHandshake, p.State())
}

// TestNoDriverIsSafe 验证未注入驱动时状态机仍可独立工作。
func TestNoDriverIsSafe(t *testing.T) {
	clock := newFakeClock()
	p, err := New(Options{NodeID: testNodeID(15), Clock: clock})
	require.NoError(t, err)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())
	require.NoError(t, p.OnKeepaliveDue())
	assert.Equal(t, StateDirect, p.State())
}

// TestOnKeepaliveDueRequestsSend 验证保活定时入口调用驱动。
func TestOnKeepaliveDueRequestsSend(t *testing.T) {
	p, _, drv := newTestPeer(t, 16)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	require.NoError(t, p.OnKeepaliveDue())
	_, _, ka := drv.counts()
	assert.Equal(t, 1, ka)
}

// TestTransitionString 覆盖审计转换的字符串渲染。
func TestTransitionString(t *testing.T) {
	tr := Transition{From: StateIdle, To: StateHandshake, Reason: "discovered"}
	assert.Equal(t, "idle -> handshake (discovered)", tr.String())
}

// TestSystemClockNow 覆盖默认时钟。
func TestSystemClockNow(t *testing.T) {
	c := SystemClock{}
	assert.False(t, c.Now().IsZero())
}

// TestPeerCandidateStorage 验证候选地址的存取与副本语义。
func TestPeerCandidateStorage(t *testing.T) {
	p, _, _ := newTestPeer(t, 20)

	// 初始为空。
	assert.Nil(t, p.RemoteCandidates())
	assert.Nil(t, p.LocalCandidates())

	rem := []nat.Candidate{
		{Type: nat.CandidateHost, Addr: netip.MustParseAddrPort("192.168.1.1:11010"), Priority: 100},
		{Type: nat.CandidateSrflx, Addr: netip.MustParseAddrPort("203.0.113.1:40000"), Priority: 50},
	}
	p.SetRemoteCandidates(rem)

	got := p.RemoteCandidates()
	require.Len(t, got, 2)
	assert.Equal(t, nat.CandidateSrflx, got[1].Type)

	// 修改副本与原切片都不应影响内部存储。
	got[0].Priority = 999
	rem[1].Priority = 888
	again := p.RemoteCandidates()
	assert.Equal(t, uint32(100), again[0].Priority)
	assert.Equal(t, uint32(50), again[1].Priority)

	// Local 独立于 Remote。
	loc := []nat.Candidate{{Type: nat.CandidateHost, Addr: netip.MustParseAddrPort("10.0.0.1:11010")}}
	p.SetLocalCandidates(loc)
	assert.Len(t, p.LocalCandidates(), 1)
	assert.Len(t, p.RemoteCandidates(), 2)
}
