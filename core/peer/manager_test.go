package peer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/session"
)

// TestManagerAddAndGet 覆盖注册、取回与替换语义。
func TestManagerAddAndGet(t *testing.T) {
	m := NewManager(ManagerOptions{Clock: newFakeClock()})
	defer m.Close()

	p, _, _ := newTestPeer(t, 1)
	replaced, err := m.Add(p)
	require.NoError(t, err)
	assert.Nil(t, replaced)

	got, ok := m.Get(p.NodeID)
	require.True(t, ok)
	assert.Same(t, p, got)
	assert.Equal(t, 1, m.Len())

	// 同 NodeID 再次 Add 应替换并关闭旧的。
	p2, _, _ := newTestPeer(t, 1)
	replaced, err = m.Add(p2)
	require.NoError(t, err)
	require.NotNil(t, replaced)
	assert.True(t, replaced.Closed(), "被替换的 Peer 应被关闭")
	assert.Equal(t, 1, m.Len())
}

// TestManagerAddNil 验证 nil Peer 被拒绝。
func TestManagerAddNil(t *testing.T) {
	m := NewManager(ManagerOptions{})
	_, err := m.Add(nil)
	assert.ErrorIs(t, err, ErrNilPeer)
}

// TestManagerGetOrCreate 验证按需创建与复用。
func TestManagerGetOrCreate(t *testing.T) {
	drv := &fakeDriver{}
	m := NewManager(ManagerOptions{Clock: newFakeClock(), Driver: drv})
	defer m.Close()

	id := testNodeID(2)
	p, err := m.GetOrCreate(id, "1.2.3.4:11010")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "1.2.3.4:11010", p.Addr())

	// 再次取回同一实例。
	again, err := m.GetOrCreate(id, "5.6.7.8:11010")
	require.NoError(t, err)
	assert.Same(t, p, again, "已存在时应复用而非重建")
	assert.Equal(t, "1.2.3.4:11010", again.Addr(), "复用不应覆盖地址")

	// 驱动应被注入到新建的 Peer 上。
	assert.Same(t, drv, p.Driver())
}

// TestManagerRemove 验证移除并关闭。
func TestManagerRemove(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	p, _, _ := newTestPeer(t, 3)
	_, err := m.Add(p)
	require.NoError(t, err)

	m.Remove(p.NodeID)
	assert.Equal(t, 0, m.Len())
	assert.True(t, p.Closed())

	// 移除不存在的项是安全的。
	m.Remove(testNodeID(99))
}

// TestManagerIDsSorted 验证 IDs 按字节序稳定排序。
func TestManagerIDsSorted(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	for _, b := range []byte{5, 1, 3} {
		p, _, _ := newTestPeer(t, b)
		_, err := m.Add(p)
		require.NoError(t, err)
	}

	ids := m.IDs()
	require.Len(t, ids, 3)
	assert.Equal(t, testNodeID(1), ids[0])
	assert.Equal(t, testNodeID(3), ids[1])
	assert.Equal(t, testNodeID(5), ids[2])
}

// TestManagerAllSorted 验证 All 返回稳定顺序。
func TestManagerAllSorted(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	for _, b := range []byte{9, 2, 6} {
		p, _, _ := newTestPeer(t, b)
		_, err := m.Add(p)
		require.NoError(t, err)
	}

	all := m.All()
	require.Len(t, all, 3)
	assert.True(t, all[0].NodeIDHex() < all[1].NodeIDHex())
	assert.True(t, all[1].NodeIDHex() < all[2].NodeIDHex())
}

// TestManagerByState 验证按状态过滤。
func TestManagerByState(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	direct, _, _ := newTestPeer(t, 1)
	require.NoError(t, direct.StartHandshake())
	require.NoError(t, direct.OnHandshakeSuccess(nil))
	require.NoError(t, direct.OnPunchSuccess())

	idle, _, _ := newTestPeer(t, 2)

	_, err := m.Add(direct)
	require.NoError(t, err)
	_, err = m.Add(idle)
	require.NoError(t, err)

	assert.Len(t, m.ByState(StateDirect), 1)
	assert.Len(t, m.ByState(StateIdle), 1)
	assert.Empty(t, m.ByState(StateDead))
}

// TestManagerTickAdvances 验证 Tick 驱动所有 Peer 的超时推进。
func TestManagerTickAdvances(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(ManagerOptions{Clock: clock})
	defer m.Close()

	p, _, _ := newTestPeer(t, 1)
	// 让 Peer 使用管理器的时钟,确保时基一致。
	p.clock = clock
	_, err := m.Add(p)
	require.NoError(t, err)

	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	clock.Advance(PeerDegradedAfter)
	assert.Equal(t, 1, m.Tick(), "应有一个 Peer 发生状态变化")
	assert.Equal(t, StateDegraded, p.State())

	// 没有新的推进时返回 0。
	assert.Equal(t, 0, m.Tick())
}

// TestManagerSessionOf 验证会话查询。
func TestManagerSessionOf(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	p, _, _ := newTestPeer(t, 1)
	_, err := m.Add(p)
	require.NoError(t, err)

	_, ok := m.SessionOf(p.NodeID)
	assert.False(t, ok, "无会话时应返回 false")

	_, ok = m.SessionOf(testNodeID(250))
	assert.False(t, ok, "不存在的 Peer 返回 false")
}

// TestManagerClosed 验证关闭后拒绝新增。
func TestManagerClosed(t *testing.T) {
	m := NewManager(ManagerOptions{})
	require.False(t, m.Closed())
	require.NoError(t, m.Close())
	assert.True(t, m.Closed())
	// 重复关闭安全。
	require.NoError(t, m.Close())

	p, _, _ := newTestPeer(t, 1)
	_, err := m.Add(p)
	assert.ErrorIs(t, err, ErrClosed)

	_, err = m.GetOrCreate(testNodeID(2), "")
	assert.ErrorIs(t, err, ErrClosed)
}

// TestManagerCloseClosesPeers 验证 Close 关闭全部 Peer。
func TestManagerCloseClosesPeers(t *testing.T) {
	m := NewManager(ManagerOptions{})

	p1, _, _ := newTestPeer(t, 1)
	p2, _, _ := newTestPeer(t, 2)
	_, err := m.Add(p1)
	require.NoError(t, err)
	_, err = m.Add(p2)
	require.NoError(t, err)

	require.NoError(t, m.Close())
	assert.True(t, p1.Closed())
	assert.True(t, p2.Closed())
	assert.Equal(t, 0, m.Len())
}

// TestManagerClock 验证时钟访问器。
func TestManagerClock(t *testing.T) {
	clock := newFakeClock()
	m := NewManager(ManagerOptions{Clock: clock})
	assert.Same(t, clock, m.Clock())

	// 默认时钟非空。
	m2 := NewManager(ManagerOptions{})
	assert.NotNil(t, m2.Clock())
}

// TestRunTickerStopsOnContext 验证定时驱动在 ctx 取消后退出。
func TestRunTickerStopsOnContext(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.RunTicker(ctx, time.Millisecond)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunTicker 未在 ctx 取消后退出")
	}
}

// TestRunTickerDrivesKeepalive 验证定时驱动会发送保活。
func TestRunTickerDrivesKeepalive(t *testing.T) {
	clock := newFakeClock()
	drv := &fakeDriver{}
	m := NewManager(ManagerOptions{Clock: clock, Driver: drv})
	defer m.Close()

	p, err := m.GetOrCreate(testNodeID(1), "1.2.3.4:11010")
	require.NoError(t, err)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunTicker(ctx, time.Millisecond)

	require.Eventually(t, func() bool {
		_, _, ka := drv.counts()
		return ka > 0
	}, time.Second, 5*time.Millisecond, "定是驱动应触发保活发送")
}

// TestRunTickerRepunch 验证 Relayed 状态下定时驱动触发重打洞。
func TestRunTickerRepunch(t *testing.T) {
	clock := newFakeClock()
	drv := &fakeDriver{}
	m := NewManager(ManagerOptions{Clock: clock, Driver: drv})
	defer m.Close()

	p, err := m.GetOrCreate(testNodeID(1), "1.2.3.4:11010")
	require.NoError(t, err)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchTimeout())
	require.Equal(t, StateRelayed, p.State())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunTicker(ctx, time.Millisecond)

	require.Eventually(t, func() bool {
		_, punch, _ := drv.counts()
		return punch >= 2 // 首次握手打洞 + 重打洞
	}, time.Second, 5*time.Millisecond, "Relayed 状态应周期重打洞")
}

// TestManagerConcurrentAccess 并发压测,弥补本环境无法运行 -race 的缺口。
func TestManagerConcurrentAccess(t *testing.T) {
	m := NewManager(ManagerOptions{Clock: newFakeClock()})
	defer m.Close()

	const workers = 16
	const iters = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := testNodeID(byte(w))
				p, err := m.GetOrCreate(id, "127.0.0.1:1")
				if err != nil {
					return
				}
				_ = p.State()
				_ = p.NodeIDHex()
				_ = m.Len()
				_ = m.IDs()
				_ = m.All()
			}
		}(w)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				m.Remove(testNodeID(byte(w)))
			}
		}(w)
	}

	wg.Wait()
}

// directPeer 构造一个处于 Direct 状态并持有会话的 Peer。
func directPeer(t *testing.T, b byte) *Peer {
	t.Helper()
	p, _, _ := newTestPeer(t, b)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())
	return p
}

// TestBroadcastSendsToActivePeers 验证 Broadcast 只发给活跃且持会话的对端。
func TestBroadcastSendsToActivePeers(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	active1 := directPeer(t, 1)
	active2 := directPeer(t, 2)
	idle, _, _ := newTestPeer(t, 3)

	// 给活跃 Peer 设置会话。
	s1, err := session.NewSession(1, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	active1.SetSession(s1)
	s2, err := session.NewSession(2, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	active2.SetSession(s2)

	for _, p := range []*Peer{active1, active2, idle} {
		_, err := m.Add(p)
		require.NoError(t, err)
	}

	var got [][crypto.NodeIDSize]byte
	sent := m.Broadcast([]byte("lsa"), func(id [crypto.NodeIDSize]byte, payload []byte) error {
		got = append(got, id)
		assert.Equal(t, "lsa", string(payload))
		return nil
	})
	assert.Equal(t, 2, sent, "只应发给两个活跃且持会话的 Peer")
	assert.Len(t, got, 2)
}

// TestBroadcastExceptSkipsGivenPeer 验证 BroadcastExcept 跳过指定对端。
func TestBroadcastExceptSkipsGivenPeer(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	active1 := directPeer(t, 1)
	active2 := directPeer(t, 2)
	for _, p := range []*Peer{active1, active2} {
		s, err := session.NewSession(1, [32]byte{1}, [32]byte{2})
		require.NoError(t, err)
		p.SetSession(s)
		_, err = m.Add(p)
		require.NoError(t, err)
	}

	var got [][crypto.NodeIDSize]byte
	sent := m.BroadcastExcept(active1.NodeID, []byte("lsa"),
		func(id [crypto.NodeIDSize]byte, payload []byte) error {
			got = append(got, id)
			return nil
		})
	assert.Equal(t, 1, sent)
	require.Len(t, got, 1)
	assert.Equal(t, active2.NodeID, got[0])
}

// TestBroadcastNilSender 验证 nil 发送函数安全返回 0。
func TestBroadcastNilSender(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()
	assert.Equal(t, 0, m.Broadcast([]byte("x"), nil))
}

// TestBroadcastSenderErrorTolerated 验证单个发送失败不影响其余对端。
func TestBroadcastSenderErrorTolerated(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	for i := byte(1); i <= 3; i++ {
		p := directPeer(t, i)
		s, err := session.NewSession(uint32(i), [32]byte{1}, [32]byte{2})
		require.NoError(t, err)
		p.SetSession(s)
		_, err = m.Add(p)
		require.NoError(t, err)
	}

	count := 0
	sent := m.Broadcast([]byte("x"), func(id [crypto.NodeIDSize]byte, payload []byte) error {
		count++
		if count == 1 {
			return errors.New("send failed")
		}
		return nil
	})
	assert.Equal(t, 2, sent, "失败一个仍应成功两个")
}

// TestBroadcastNoSessionSkipped 验证无会话的 Direct Peer 被跳过。
func TestBroadcastNoSessionSkipped(t *testing.T) {
	m := NewManager(ManagerOptions{})
	defer m.Close()

	p := directPeer(t, 1) // Direct 但无会话
	_, err := m.Add(p)
	require.NoError(t, err)

	sent := m.Broadcast([]byte("x"), func(id [crypto.NodeIDSize]byte, payload []byte) error {
		return nil
	})
	assert.Equal(t, 0, sent)
}
