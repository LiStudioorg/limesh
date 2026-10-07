package session

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Manager 是按会话 ID 索引的会话池。
//
// 并发安全:所有方法都可由多个 goroutine 并发调用。会话本身也有独立锁,
// 因此"从池中取出会话后继续使用"期间无需持有池锁——这避免了长耗时的
// 加解密操作阻塞其它会话的查找。
type Manager struct {
	mu       sync.RWMutex
	sessions map[uint32]*Session
	// peerIndex 提供从对端 NodeID 到会话 ID 的反向索引。
	// 同一对端在同一时刻只应有一个活跃会话,轮换时以新代旧。
	peerIndex map[[16]byte]uint32
	// closed 标记管理器已关闭。
	closed bool
}

// NewManager 创建一个空的会话池。
func NewManager() *Manager {
	return &Manager{
		sessions:  make(map[uint32]*Session),
		peerIndex: make(map[[16]byte]uint32),
	}
}

// Add 把会话加入池中。
//
// 若池中已存在同一对端的会话(密钥轮换、重连或双向同时握手的结果),
// 旧会话会被关闭并从池中移除,保证"一个对端一个活跃会话"这一不变量。
// 返回被替换掉的旧会话,若没有则为 nil。
func (m *Manager) Add(s *Session) (*Session, error) {
	if s == nil {
		return nil, fmt.Errorf("session: manager add: nil session")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("session: manager add %d: %w", s.ID, ErrSessionClosed)
	}

	var replaced *Session

	// 同一 ID 已存在:直接替换(通常意味着重复添加)。
	if old, ok := m.sessions[s.ID]; ok && old != s {
		replaced = old
	}

	// 同一对端已有其它会话:也替换,避免同对端出现两条会话导致
	// 序号空间冲突与解密歧义。
	if s.PeerID != ([16]byte{}) {
		if oldID, ok := m.peerIndex[s.PeerID]; ok && oldID != s.ID {
			if old, ok := m.sessions[oldID]; ok {
				replaced = old
				delete(m.sessions, oldID)
			}
		}
		m.peerIndex[s.PeerID] = s.ID
	}

	m.sessions[s.ID] = s

	if replaced != nil && replaced != s {
		// 在锁外关闭更安全,但此处旧会话已从索引中摘除,
		// 关闭它只是清零密钥,不会与其它操作竞争池状态。
		replaced.Close()
		return replaced, nil
	}
	return nil, nil
}

// Get 按键取回会话。
func (m *Manager) Get(id uint32) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[id]
	return s, ok
}

// GetByPeer 按对端 NodeID 取回会话。
func (m *Manager) GetByPeer(peerID [16]byte) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	id, ok := m.peerIndex[peerID]
	if !ok {
		return nil, false
	}
	s, ok := m.sessions[id]
	return s, ok
}

// Remove 移除并关闭指定会话。
func (m *Manager) Remove(id uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok {
		return
	}
	delete(m.sessions, id)

	// 只在反向索引仍指向该会话时才清理,避免误删已被新会话接管的索引。
	if s.PeerID != ([16]byte{}) {
		if cur, ok := m.peerIndex[s.PeerID]; ok && cur == id {
			delete(m.peerIndex, s.PeerID)
		}
	}
	s.Close()
}

// RemoveByPeer 移除指定对端的所有会话。
func (m *Manager) RemoveByPeer(peerID [16]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id, ok := m.peerIndex[peerID]
	if !ok {
		return
	}
	delete(m.peerIndex, peerID)

	if s, ok := m.sessions[id]; ok {
		delete(m.sessions, id)
		s.Close()
	}
}

// Len 返回当前池中的会话数。
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// IDs 返回池中所有会话 ID,已排序。
//
// 排序保证输出稳定,便于测试与日志比对。
func (m *Manager) IDs() []uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]uint32, 0, len(m.sessions))
	for id := range m.sessions {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// All 返回池中所有会话的快照。
func (m *Manager) All() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// GC 回收已过期的会话,返回被回收的会话数。
//
// 参数 olderThan 为 0 时使用每个会话自身的 TTL。
//
// 过期判定采用"最后活动时间"而非"创建时间":一条持续传输的会话不应因为
// 建立时间早而被回收。
func (m *Manager) GC(olderThan time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return 0
	}

	var removed int
	for id, s := range m.sessions {
		expired := s.Expired()
		if !expired && olderThan > 0 {
			expired = s.Idle() > olderThan
		}
		if !expired {
			continue
		}

		delete(m.sessions, id)
		if s.PeerID != ([16]byte{}) {
			if cur, ok := m.peerIndex[s.PeerID]; ok && cur == id {
				delete(m.peerIndex, s.PeerID)
			}
		}
		s.Close()
		removed++
	}
	return removed
}

// RunGC 周期性执行 GC,直到 ctx 结束。
//
// interval 为 0 时使用 1 分钟。返回时不会关闭会话池,由调用方决定是否 Close。
func (m *Manager) RunGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.GC(0)
		}
	}
}

// Close 关闭池中所有会话并清空索引。重复调用是安全的。
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	for id, s := range m.sessions {
		s.Close()
		delete(m.sessions, id)
	}
	for k := range m.peerIndex {
		delete(m.peerIndex, k)
	}
	return nil
}

// Closed 报告管理器是否已关闭。
func (m *Manager) Closed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}
