package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSession 构造一个带指定 ID 与对端身份的会话。
func newTestSession(t *testing.T, id uint32, peer [16]byte) *Session {
	t.Helper()
	s, err := NewSession(id, randomKey(t), randomKey(t))
	require.NoError(t, err)
	s.PeerID = peer
	return s
}

func TestManagerAddAndGet(t *testing.T) {
	m := NewManager()
	defer m.Close()

	s := newTestSession(t, 1, [16]byte{1})
	replaced, err := m.Add(s)
	require.NoError(t, err)
	assert.Nil(t, replaced, "首次添加不应替换任何会话")

	got, ok := m.Get(1)
	require.True(t, ok)
	assert.Same(t, s, got)
	assert.Equal(t, 1, m.Len())
}

func TestManagerAddNil(t *testing.T) {
	m := NewManager()
	defer m.Close()

	_, err := m.Add(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil session")
}

func TestManagerGetMissing(t *testing.T) {
	m := NewManager()
	defer m.Close()

	_, ok := m.Get(999)
	assert.False(t, ok)

	_, ok = m.GetByPeer([16]byte{9})
	assert.False(t, ok)
}

func TestManagerGetByPeer(t *testing.T) {
	m := NewManager()
	defer m.Close()

	peer := [16]byte{0xAA, 0xBB}
	s := newTestSession(t, 7, peer)
	_, err := m.Add(s)
	require.NoError(t, err)

	got, ok := m.GetByPeer(peer)
	require.True(t, ok)
	assert.Same(t, s, got)
}

func TestManagerReplacesSamePeerSession(t *testing.T) {
	// 一个对端在同一时刻只应有一个活跃会话:重连或密钥轮换时以新代旧,
	// 否则同对端会有两条会话,序号空间冲突会导致解密歧义。
	m := NewManager()
	defer m.Close()

	peer := [16]byte{0x01}

	old := newTestSession(t, 1, peer)
	_, err := m.Add(old)
	require.NoError(t, err)

	fresh := newTestSession(t, 2, peer)
	replaced, err := m.Add(fresh)
	require.NoError(t, err)

	require.NotNil(t, replaced, "同对端的新会话应替换旧会话")
	assert.Same(t, old, replaced)
	assert.True(t, old.Closed(), "被替换的会话应被关闭并清零密钥")

	got, ok := m.GetByPeer(peer)
	require.True(t, ok)
	assert.Same(t, fresh, got)

	_, ok = m.Get(1)
	assert.False(t, ok, "旧会话应从索引中移除")
	assert.Equal(t, 1, m.Len())
}

func TestManagerReplacesSameID(t *testing.T) {
	m := NewManager()
	defer m.Close()

	first := newTestSession(t, 5, [16]byte{1})
	_, err := m.Add(first)
	require.NoError(t, err)

	second := newTestSession(t, 5, [16]byte{2})
	replaced, err := m.Add(second)
	require.NoError(t, err)

	require.NotNil(t, replaced)
	assert.Same(t, first, replaced)
	assert.True(t, first.Closed())

	got, ok := m.Get(5)
	require.True(t, ok)
	assert.Same(t, second, got)
}

func TestManagerReaddingSameSessionIsIdempotent(t *testing.T) {
	m := NewManager()
	defer m.Close()

	s := newTestSession(t, 3, [16]byte{3})
	_, err := m.Add(s)
	require.NoError(t, err)

	replaced, err := m.Add(s)
	require.NoError(t, err)
	assert.Nil(t, replaced, "重复添加同一实例不应把自己替换掉")
	assert.False(t, s.Closed(), "会话不应被自己关闭")

	got, ok := m.Get(3)
	require.True(t, ok)
	assert.Same(t, s, got)
}

func TestManagerSessionWithoutPeerID(t *testing.T) {
	// PeerID 为零值时跳过反向索引,但仍可通过 ID 检索。
	m := NewManager()
	defer m.Close()

	s := newTestSession(t, 11, [16]byte{})
	_, err := m.Add(s)
	require.NoError(t, err)

	got, ok := m.Get(11)
	require.True(t, ok)
	assert.Same(t, s, got)
}

func TestManagerRemove(t *testing.T) {
	m := NewManager()
	defer m.Close()

	s := newTestSession(t, 1, [16]byte{1})
	_, err := m.Add(s)
	require.NoError(t, err)

	m.Remove(1)

	_, ok := m.Get(1)
	assert.False(t, ok)
	_, ok = m.GetByPeer([16]byte{1})
	assert.False(t, ok, "反向索引也应被清理")
	assert.True(t, s.Closed())
	assert.Zero(t, m.Len())
}

func TestManagerRemoveMissingIsNoop(t *testing.T) {
	m := NewManager()
	defer m.Close()

	assert.NotPanics(t, func() { m.Remove(42) })
	assert.Zero(t, m.Len())
}

func TestManagerRemoveByPeer(t *testing.T) {
	m := NewManager()
	defer m.Close()

	peer := [16]byte{0x77}
	s := newTestSession(t, 8, peer)
	_, err := m.Add(s)
	require.NoError(t, err)

	m.RemoveByPeer(peer)

	_, ok := m.GetByPeer(peer)
	assert.False(t, ok)
	_, ok = m.Get(8)
	assert.False(t, ok, "按对端移除也应清理 ID 索引")
	assert.True(t, s.Closed())
}

func TestManagerRemoveByPeerMissingIsNoop(t *testing.T) {
	m := NewManager()
	defer m.Close()

	assert.NotPanics(t, func() { m.RemoveByPeer([16]byte{9}) })
}

func TestManagerIDsAreSorted(t *testing.T) {
	// 稳定的输出顺序便于测试与日志比对。
	m := NewManager()
	defer m.Close()

	for _, id := range []uint32{30, 10, 20, 5} {
		_, err := m.Add(newTestSession(t, id, [16]byte{byte(id)}))
		require.NoError(t, err)
	}

	assert.Equal(t, []uint32{5, 10, 20, 30}, m.IDs())
}

func TestManagerAll(t *testing.T) {
	m := NewManager()
	defer m.Close()

	assert.Empty(t, m.All())

	for _, id := range []uint32{1, 2, 3} {
		_, err := m.Add(newTestSession(t, id, [16]byte{byte(id)}))
		require.NoError(t, err)
	}
	assert.Len(t, m.All(), 3)
}

// ---------------------------------------------------------------------------
// GC
// ---------------------------------------------------------------------------

func TestManagerGCBySessionTTL(t *testing.T) {
	m := NewManager()
	defer m.Close()

	shortLived, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), 20*time.Millisecond)
	require.NoError(t, err)
	shortLived.PeerID = [16]byte{1}

	longLived, err := NewSessionWithTTL(2, randomKey(t), randomKey(t), time.Hour)
	require.NoError(t, err)
	longLived.PeerID = [16]byte{2}

	_, err = m.Add(shortLived)
	require.NoError(t, err)
	_, err = m.Add(longLived)
	require.NoError(t, err)

	time.Sleep(40 * time.Millisecond)

	assert.Equal(t, 1, m.GC(0), "应回收 1 个过期会话")
	assert.True(t, shortLived.Closed(), "被回收的会话应被关闭")

	_, ok := m.Get(2)
	assert.True(t, ok, "未过期的会话应保留")
	_, ok = m.GetByPeer([16]byte{1})
	assert.False(t, ok, "被回收会话的反向索引应清理")
}

func TestManagerGCWithExplicitThreshold(t *testing.T) {
	m := NewManager()
	defer m.Close()

	s, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), time.Hour)
	require.NoError(t, err)
	_, err = m.Add(s)
	require.NoError(t, err)

	time.Sleep(30 * time.Millisecond)

	// 显式阈值比会话自身 TTL 更严格时应生效。
	assert.Equal(t, 1, m.GC(10*time.Millisecond), "显式阈值应收紧回收条件")
	assert.True(t, s.Closed())
}

func TestManagerGCKeepsActiveSessions(t *testing.T) {
	m := NewManager()
	defer m.Close()

	s, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), 50*time.Millisecond)
	require.NoError(t, err)
	_, err = m.Add(s)
	require.NoError(t, err)

	// 持续活动:不应被回收。这验证了判定基于"最后活动时间"而非"创建时间"。
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		s.Touch()
		assert.Equal(t, 0, m.GC(0), "活跃会话不应被回收")
	}
	assert.False(t, s.Closed())
}

func TestManagerRunGCStopsOnContextCancel(t *testing.T) {
	m := NewManager()
	defer m.Close()

	_, err := m.Add(newTestSession(t, 1, [16]byte{1}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		m.RunGC(ctx, 10*time.Millisecond)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunGC 未在 ctx 取消后返回")
	}
}

// ---------------------------------------------------------------------------
// 关闭语义
// ---------------------------------------------------------------------------

func TestManagerCloseClosesAllSessions(t *testing.T) {
	m := NewManager()

	s1 := newTestSession(t, 1, [16]byte{1})
	s2 := newTestSession(t, 2, [16]byte{2})
	_, err := m.Add(s1)
	require.NoError(t, err)
	_, err = m.Add(s2)
	require.NoError(t, err)

	require.NoError(t, m.Close())

	assert.True(t, s1.Closed())
	assert.True(t, s2.Closed())
	assert.Zero(t, m.Len())
	assert.True(t, m.Closed())
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	m := NewManager()

	require.NoError(t, m.Close())
	require.NoError(t, m.Close(), "重复 Close 应安全")
}

func TestManagerAddAfterCloseFails(t *testing.T) {
	m := NewManager()
	require.NoError(t, m.Close())

	_, err := m.Add(newTestSession(t, 1, [16]byte{1}))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSessionClosed)
}

func TestManagerGCAfterCloseIsNoop(t *testing.T) {
	m := NewManager()
	require.NoError(t, m.Close())

	assert.Zero(t, m.GC(0))
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestManagerConcurrentAccess(t *testing.T) {
	// -race 下必须无报告。
	m := NewManager()
	defer m.Close()

	const workers = 8
	const ops = 100

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			for i := 0; i < ops; i++ {
				id := uint32(w*ops + i)
				peer := [16]byte{byte(id), byte(id >> 8)}

				s, err := NewSession(id, randomKey(t), randomKey(t))
				if err != nil {
					t.Errorf("new session: %v", err)
					return
				}
				s.PeerID = peer

				_, _ = m.Add(s)
				m.Get(id)
				m.GetByPeer(peer)
				m.Len()
				m.IDs()
				m.All()

				if i%3 == 0 {
					m.Remove(id)
				}
				if i%7 == 0 {
					m.GC(0)
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestManagerConcurrentAddSamePeer(t *testing.T) {
	// 多个 goroutine 争抢同一对端:最终只能有一个存活会话。
	m := NewManager()
	defer m.Close()

	peer := [16]byte{0x55}

	const workers = 16
	var wg sync.WaitGroup

	sessions := make([]*Session, workers)
	for w := 0; w < workers; w++ {
		s, err := NewSession(uint32(w+1), randomKey(t), randomKey(t))
		require.NoError(t, err)
		s.PeerID = peer
		sessions[w] = s
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_, _ = m.Add(sessions[w])
		}(w)
	}
	wg.Wait()

	// 池中应恰好剩一个会话,且反向索引与它一致。
	assert.Equal(t, 1, m.Len(), "同一对端只应保留一个会话")

	kept, ok := m.GetByPeer(peer)
	require.True(t, ok)

	_, ok = m.Get(kept.ID)
	assert.True(t, ok, "反向索引应指向池中存在的会话")

	// 除保留者外,其余会话都应被关闭。
	closed := 0
	for _, s := range sessions {
		if s.Closed() {
			closed++
		}
	}
	assert.Equal(t, workers-1, closed, "被替换的会话应全部关闭")
}
