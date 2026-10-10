package peer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/session"
)

// Manager 是全量 Peer 的注册表与定时驱动的宿主。
//
// 并发安全:所有方法都可由多个 goroutine 并发调用。Peer 自带独立锁,
// 因此"取出 Peer 后继续使用"期间无需持有 Manager 锁。
type Manager struct {
	mu    sync.RWMutex
	peers map[[crypto.NodeIDSize]byte]*Peer
	// clock 是注入的时钟,所有 Peer 与定时驱动共享同一时基。
	clock Clock
	// driver 是构造新 Peer 时注入的默认驱动。
	driver Driver
	// closed 标记管理器已关闭。
	closed bool
}

// ManagerOptions 是构造 Manager 的参数。
type ManagerOptions struct {
	// Clock 是注入的时钟;为空时使用 SystemClock。
	Clock Clock
	// Driver 是构造新 Peer 时注入的默认驱动,可为 nil。
	Driver Driver
}

// NewManager 创建一个空的 Peer 注册表。
func NewManager(opts ManagerOptions) *Manager {
	clock := opts.Clock
	if clock == nil {
		clock = SystemClock{}
	}
	return &Manager{
		peers:  make(map[[crypto.NodeIDSize]byte]*Peer),
		clock:  clock,
		driver: opts.Driver,
	}
}

// Clock 返回管理器使用的时钟,供上层构造 Peer 时复用同一时基。
func (m *Manager) Clock() Clock { return m.clock }

// Add 注册一个 Peer。若同 NodeID 已存在,替换并关闭旧的。
//
// 返回被替换掉的旧 Peer,若没有则为 nil。
func (m *Manager) Add(p *Peer) (*Peer, error) {
	if p == nil {
		return nil, ErrNilPeer
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("peer: manager add %s: %w", p.NodeIDHex(), ErrClosed)
	}

	var replaced *Peer
	if old, ok := m.peers[p.NodeID]; ok && old != p {
		replaced = old
	}
	m.peers[p.NodeID] = p

	if replaced != nil {
		// 在锁外关闭更省时,但旧 Peer 已从索引摘除,关闭只影响自身。
		replaced.Close()
		return replaced, nil
	}
	return nil, nil
}

// Get 按 NodeID 取回 Peer。
func (m *Manager) Get(id [crypto.NodeIDSize]byte) (*Peer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.peers[id]
	return p, ok
}

// GetOrCreate 取回已存在的 Peer,不存在则按 addr 创建一个。
//
// 创建时复用 Manager 的时钟与默认驱动,保证时基与动作注入一致。
func (m *Manager) GetOrCreate(id [crypto.NodeIDSize]byte, addr string) (*Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, fmt.Errorf("peer: manager get or create %s: %w", crypto.NodeIDString(id), ErrClosed)
	}
	if p, ok := m.peers[id]; ok {
		return p, nil
	}

	p, err := New(Options{
		NodeID: id,
		Addr:   addr,
		Clock:  m.clock,
		Driver: m.driver,
	})
	if err != nil {
		return nil, err
	}
	m.peers[id] = p
	return p, nil
}

// Remove 移除并关闭指定 Peer。
func (m *Manager) Remove(id [crypto.NodeIDSize]byte) {
	m.mu.Lock()
	p, ok := m.peers[id]
	delete(m.peers, id)
	m.mu.Unlock()

	if ok && p != nil {
		p.Close()
	}
}

// Len 返回当前注册的 Peer 数量。
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.peers)
}

// IDs 返回所有 Peer 的 NodeID,已按字节序排序,保证输出稳定。
func (m *Manager) IDs() [][crypto.NodeIDSize]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([][crypto.NodeIDSize]byte, 0, len(m.peers))
	for id := range m.peers {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		for k := 0; k < crypto.NodeIDSize; k++ {
			if out[i][k] != out[j][k] {
				return out[i][k] < out[j][k]
			}
		}
		return false
	})
	return out
}

// All 返回所有 Peer 的快照。
func (m *Manager) All() []*Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].NodeIDHex() < out[j].NodeIDHex()
	})
	return out
}

// ByState 返回按状态分组的 Peer 列表。
//
// 顺序经排序,便于拓扑 API 与日志的稳定输出。
func (m *Manager) ByState(state State) []*Peer {
	var out []*Peer
	for _, p := range m.All() {
		if p.State() == state {
			out = append(out, p)
		}
	}
	return out
}

// SessionOf 返回指定 Peer 的活跃会话。
func (m *Manager) SessionOf(id [crypto.NodeIDSize]byte) (*session.Session, bool) {
	p, ok := m.Get(id)
	if !ok {
		return nil, false
	}
	s := p.Session()
	return s, s != nil
}

// Tick 对所有 Peer 执行一次超时推进,返回发生状态变化的 Peer 数量。
//
// 上层可周期性调用它(例如每次保活唤醒时),也可由 RunTicker 自动驱动。
func (m *Manager) Tick() int {
	var changed int
	for _, p := range m.All() {
		if ok, _ := p.Tick(); ok {
			changed++
		}
	}
	return changed
}

// RunTicker 周期性执行 Tick、保活与重新打洞,直到 ctx 结束。
//
// interval 为 0 时使用 KeepaliveInterval。所有 Peer 处于同一驱动循环中,
// 因此它们共享同一时基,超时判定彼此可比。
func (m *Manager) RunTicker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = KeepaliveInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.driveOnce()
		}
	}
}

// driveOnce 是单次定时驱动:先推进超时,再做保活与重打洞。
func (m *Manager) driveOnce() {
	for _, p := range m.All() {
		// 超时推进优先级最高:进入 Dead 后不再保活,避免无谓流量。
		if changed, _ := p.Tick(); changed && p.State() == StateDead {
			continue
		}

		if p.ShouldRepunch() {
			_ = p.Repunch()
			continue
		}

		if p.State() == StateDirect || p.State() == StateRelayed {
			_ = p.OnKeepaliveDue()
		}
	}
}

// Close 关闭所有 Peer 并清空注册表。重复调用是安全的。
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

	peers := make([]*Peer, 0, len(m.peers))
	for _, p := range m.peers {
		peers = append(peers, p)
	}
	for id := range m.peers {
		delete(m.peers, id)
	}
	m.mu.Unlock()

	for _, p := range peers {
		p.Close()
	}
	return nil
}

// Closed 报告管理器是否已关闭。
func (m *Manager) Closed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}
