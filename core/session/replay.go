// Package session 提供加密会话、抗重放保护与会话池管理。
//
// 职责边界:会话负责 **已建立密钥之上的加解密与重放防护**,不参与握手。
// 密钥协商见 core/handshake,身份与密钥持久化见 core/crypto。
//
// 设计说明见 AGENTS.md 8.5(抗重放)、8.7(数据协议)与 8.10(安全考虑)。
package session

import (
	"fmt"
	"sync"
)

// 抗重放窗口参数。
const (
	// DefaultWindowSize 是默认窗口位数。
	//
	// 1024 位的选择依据:窗口需覆盖"最坏情况下的乱序深度"。在一个 MTU 1380
	// 的链路上,1024 位约等于 1.4 MB 的在途数据,足以吸收多路径并行造成的
	// 乱序,同时保持位图仅 128 字节。
	DefaultWindowSize = 1024

	// wordBits 是单个位图字的位数(uint64)。
	wordBits = 64

	// maxWindowShift 是单次允许的最大前跳距离。
	//
	// 防止恶意构造的超大序号一次性把整个位图清零并推进到极远处,
	// 那会让所有真实在途包变成"过老"而被丢弃。
	maxWindowShift = DefaultWindowSize
)

// ReplayWindow 实现 RFC 6479 风格的滑动位图抗重放窗口。
//
// 算法要点:
//   - 维护 `max`(已接受的最大序号)与一张循环位图;
//   - 对每个到达的序号,先判断它相对 max 的位置(过老 / 窗口内 / 过新);
//   - 窗口内且未置位 → 接受;已置位 → 重放;过老 → 丢弃。
//
// 位图按 `seq/windowSize` 分块,块号变化时只需清零"跨越的部分"而非整个位图,
// 这正是 RFC 6479 相对"每位一个时间戳"的优化所在。
//
// 并发安全:所有公开方法都由内部互斥锁保护。
type ReplayWindow struct {
	mu   sync.Mutex
	size uint64

	// bitmap 是循环位图,长度为 size/wordBits 个字。
	bitmap []uint64
	// max 是已接受的最大序号。首次接受前为 0 且 last 为 false。
	max uint64
	// last 记录是否已接受过任何序号。
	last bool
}

// NewReplayWindow 创建一个指定位宽的抗重放窗口。
//
// size 必须是 64 的正整数倍且不超过 1<<20,否则返回错误——窗口过小会让正常
// 乱序被误判为重放,过大则浪费内存且无实际收益。
func NewReplayWindow(size int) (*ReplayWindow, error) {
	if size <= 0 {
		return nil, fmt.Errorf("session: new replay window: size %d must be positive", size)
	}
	if size%wordBits != 0 {
		return nil, fmt.Errorf("session: new replay window: size %d must be a multiple of %d", size, wordBits)
	}
	if size > 1<<20 {
		return nil, fmt.Errorf("session: new replay window: size %d exceeds maximum %d", size, 1<<20)
	}

	return &ReplayWindow{
		size:   uint64(size),
		bitmap: make([]uint64, size/wordBits),
	}, nil
}

// MustReplayWindow 与 NewReplayWindow 相同,但参数非法时 panic。
//
// 仅用于参数为编译期常量的场景(如 DefaultWindowSize)。
func MustReplayWindow(size int) *ReplayWindow {
	w, err := NewReplayWindow(size)
	if err != nil {
		panic(err)
	}
	return w
}

// Check 判断序号 seq 是否可接受,但不修改窗口状态。
//
// 返回 true 表示该序号未被见过,可以接受;false 表示重放或过老,必须丢弃。
//
// Check 与 Advance 分离,使调用方可以先解密验证再提交状态——若解密失败
// (例如密文被篡改),不应占用该序号,否则会造成合法的重传被误判为重放。
func (w *ReplayWindow) Check(seq uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkLocked(seq)
}

// checkLocked 是 Check 的无锁实现,调用方必须已持有 w.mu。
func (w *ReplayWindow) checkLocked(seq uint64) bool {
	// 尚未接受过任何包:任何序号都是新的。
	//
	// 注意这里不做 maxWindowShift 限制:首个包可能因对端重启或长时间静默而
	// 携带较大序号,拒绝它会导致会话永远无法开始。
	if !w.last {
		return true
	}

	// 序号超过当前窗口上界。
	if seq > w.max {
		// 允许前跳,但限制单次跳跃距离,防止恶意序号一次性掏空窗口。
		return seq-w.max <= maxWindowShift
	}

	// 落后 max 超过一个窗口宽度:过老,无法判断是否重放,一律丢弃。
	if w.max-seq >= w.size {
		return false
	}

	// 窗口内:检查对应位是否已置位。
	index := seq % w.size
	word := index / wordBits
	bit := index % wordBits

	return w.bitmap[word]&(1<<bit) == 0
}

// Advance 接受序号 seq,把窗口推进到包含它。
//
// 调用方应先通过 Check 确认可接受。对未通过 Check 的序号调用 Advance 会
// 破坏窗口状态,因此这是调用方的责任。
//
// 若 seq 已经超出可推进范围(过老),Advance 静默忽略,不改变任何状态。
func (w *ReplayWindow) Advance(seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.last {
		w.last = true
		w.max = seq
		w.setBitLocked(seq)
		return
	}

	if seq > w.max {
		w.shiftLocked(seq)
		w.max = seq
		w.setBitLocked(seq)
		return
	}

	// 窗口内或过老。过老的序号不应影响状态。
	if w.max-seq >= w.size {
		return
	}
	w.setBitLocked(seq)
}

// shiftLocked 把位图向前推进,使 seq 落入窗口。
//
// 推进时只需清零"新覆盖区间"对应的字,而非整个位图:这保留了仍在窗口内的
// 历史位,使稍后到达的乱序包仍能被正确识别。
func (w *ReplayWindow) shiftLocked(seq uint64) {
	shift := seq - w.max
	if shift >= w.size {
		// 跳跃超过一整个窗口:所有历史位都已无意义,直接清零。
		for i := range w.bitmap {
			w.bitmap[i] = 0
		}
		return
	}

	// 从旧上界到新上界,逐位清零被推出窗口的槽位。
	for s := w.max + 1; s <= seq; s++ {
		index := s % w.size
		word := index / wordBits
		bit := index % wordBits
		w.bitmap[word] &^= 1 << bit
	}
}

// setBitLocked 置位序号 seq 对应的位。
func (w *ReplayWindow) setBitLocked(seq uint64) {
	index := seq % w.size
	word := index / wordBits
	bit := index % wordBits
	w.bitmap[word] |= 1 << bit
}

// Accept 是 Check + Advance 的原子组合。
//
// 返回 true 表示该序号已被接受并占用。适用于不需要"先验证后提交"两阶段的
// 场景(例如控制消息)。
func (w *ReplayWindow) Accept(seq uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.checkLocked(seq) {
		return false
	}

	// 内联 Advance 的无锁版本,保证检查与提交在同一临界区内。
	if !w.last {
		w.last = true
		w.max = seq
	} else if seq > w.max {
		w.shiftLocked(seq)
		w.max = seq
	}
	w.setBitLocked(seq)
	return true
}

// Max 返回已接受的最大序号。
func (w *ReplayWindow) Max() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.max
}

// Size 返回窗口位宽。
func (w *ReplayWindow) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int(w.size)
}

// Initialized 报告窗口是否已接受过至少一个序号。
func (w *ReplayWindow) Initialized() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// Reset 清空窗口状态。
//
// 用于密钥轮换:新密钥的序号空间独立于旧密钥,必须重新开始计数,
// 否则新密钥的前若干个包会被误判为过老。
func (w *ReplayWindow) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := range w.bitmap {
		w.bitmap[i] = 0
	}
	w.max = 0
	w.last = false
}
