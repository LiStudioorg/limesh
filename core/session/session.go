package session

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// 密钥与 nonce 长度。ChaCha20-Poly1305 使用 32 字节密钥、12 字节 nonce。
const (
	// KeySize 是 AEAD 密钥长度。
	KeySize = chacha20poly1305.KeySize
	// NonceSize 是 AEAD nonce 长度。
	NonceSize = chacha20poly1305.NonceSize
	// TagSize 是 Poly1305 认证标签长度。
	TagSize = chacha20poly1305.Overhead
)

// 会话默认参数。
const (
	// DefaultSessionTTL 是会话的默认存活时间。
	//
	// 与 security.key_rotation.interval 的默认值(168h)不同:该 TTL 只是
	// **空闲回收**阈值,防止长期不活跃的会话永久占用内存,而非密钥生命周期。
	DefaultSessionTTL = 10 * time.Minute

	// DefaultMaxSendSeq 是单个会话允许发送的最大包数。
	//
	// ChaCha20-Poly1305 的 nonce 由 SessionID + 序号构造,nonce 重用会彻底
	// 破坏机密性。64 位序号在实践中不可能耗尽,但显式设限使"密钥轮换"有
	// 明确的触发条件,而不是依赖一个不会被触及的上界。
	DefaultMaxSendSeq = uint64(1) << 62
)

// 会话相关错误。
var (
	// ErrSessionExpired 表示会话已超过存活时间。
	ErrSessionExpired = errors.New("session: expired")
	// ErrSessionClosed 表示会话已被显式关闭。
	ErrSessionClosed = errors.New("session: closed")
	// ErrReplay 表示收到的包是重放或过老。
	ErrReplay = errors.New("session: replay detected")
	// ErrDecrypt 表示认证解密失败。
	ErrDecrypt = errors.New("session: decrypt failed")
	// ErrEncrypt 表示加密失败。
	ErrEncrypt = errors.New("session: encrypt failed")
	// ErrSeqExhausted 表示会话发送序号已耗尽,必须轮换密钥。
	ErrSeqExhausted = errors.New("session: send sequence exhausted")
	// ErrShortPacket 表示密文长度不足以包含认证标签。
	ErrShortPacket = errors.New("session: ciphertext too short")
	// ErrKeySize 表示密钥长度不合法。
	ErrKeySize = errors.New("session: invalid key size")
)

// Session 是一条已建立密钥的单向加密会话。
//
// 与会话的"双向"概念区分:一条 Session 只描述**一个方向**的发送密钥与
// 对端方向的接收密钥。这样做是因为 Noise 的 CipherState 对天然是分方向的,
// 用单条 Session 承载双向会导致加解密使用同一 nonce 空间,是严重的安全错误。
type Session struct {
	// ID 是会话标识,写入 ZCPacket.SessionID 字段。
	ID uint32
	// PeerID 是对端 NodeID。
	PeerID [16]byte
	// CreatedAt 是会话建立时间。
	CreatedAt time.Time

	// sendKey / recvKey 分别为本端发送与接收方向的 AEAD 密钥。
	sendKey [KeySize]byte
	recvKey [KeySize]byte

	// sendAEAD / recvAEAD 是预构造的 AEAD 实例。
	// 复用以避免每包重建带来的分配。
	sendAEAD cipher.AEAD
	recvAEAD cipher.AEAD

	// recvWindow 是针对接收方向序号的抗重放窗口。
	recvWindow *ReplayWindow

	mu sync.Mutex
	// sendSeq 是下一个待分配的发送序号,由 NextSequence 递增。
	sendSeq uint64
	// lastSent 是已实际加密过的最大序号 + 1(即"已用序号 + 1")。
	//
	// 与 sendSeq 分开维护是必要的:NextSequence 只是**预留**了序号,
	// 调用方可能预留后并不立即加密(甚至永不加密)。nonce 重用的判定
	// 必须以"真正加过密的序号"为准,否则会误判合法流程,或在预留与
	// 加密交错时漏判真正的重用。
	lastSent uint64
	// lastUsed 记录最后一次收发时间,用于空闲回收。
	lastUsed time.Time
	// ttl 是该会话的空闲存活时间。
	ttl time.Duration
	// closed 标记会话已关闭。
	closed bool
}

// NewSession 使用给定的发送/接收密钥创建会话。
//
// 密钥方向约定:sendKey 用于本端加密,recvKey 用于本端解密。调用方必须保证
// 两端的 (sendKey, recvKey) 是交叉对应的,即本端的 sendKey 等于对端的 recvKey
// ——Noise 的 CipherState 对已经满足这一性质,直接按序传入即可。
func NewSession(id uint32, sendKey, recvKey [KeySize]byte) (*Session, error) {
	sendAEAD, err := chacha20poly1305.New(sendKey[:])
	if err != nil {
		return nil, fmt.Errorf("session: new %d: send cipher: %w: %w", id, ErrKeySize, err)
	}
	recvAEAD, err := chacha20poly1305.New(recvKey[:])
	if err != nil {
		return nil, fmt.Errorf("session: new %d: recv cipher: %w: %w", id, ErrKeySize, err)
	}

	now := time.Now()
	return &Session{
		ID:         id,
		CreatedAt:  now,
		sendKey:    sendKey,
		recvKey:    recvKey,
		sendAEAD:   sendAEAD,
		recvAEAD:   recvAEAD,
		recvWindow: MustReplayWindow(DefaultWindowSize),
		lastUsed:   now,
		ttl:        DefaultSessionTTL,
	}, nil
}

// NewSessionWithTTL 与 NewSession 相同,但显式指定空闲存活时间。
func NewSessionWithTTL(id uint32, sendKey, recvKey [KeySize]byte, ttl time.Duration) (*Session, error) {
	s, err := NewSession(id, sendKey, recvKey)
	if err != nil {
		return nil, err
	}
	if ttl > 0 {
		s.ttl = ttl
	}
	return s, nil
}

// SendKey 返回发送方向密钥的副本。
func (s *Session) SendKey() [KeySize]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendKey
}

// RecvKey 返回接收方向密钥的副本。
func (s *Session) RecvKey() [KeySize]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvKey
}

// NextSequence 返回并递增发送序号。
//
// 序号是 nonce 的一半,必须单调递增且不重复。调用方应在 Seal 之前取得序号,
// 并把同一个序号写入 ZCPacket.Sequence,使接收方能以相同 nonce 解开。
func (s *Session) NextSequence() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, fmt.Errorf("session: next sequence %d: %w", s.ID, ErrSessionClosed)
	}
	if s.sendSeq >= DefaultMaxSendSeq {
		return 0, fmt.Errorf("session: next sequence %d: %w", s.ID, ErrSeqExhausted)
	}

	seq := s.sendSeq
	s.sendSeq++
	s.lastUsed = time.Now()
	return seq, nil
}

// Sequence 返回当前发送序号(即将使用的那个),不递增。
func (s *Session) Sequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendSeq
}

// Encrypt 使用指定序号加密明文。
//
// 调用方负责分配序号(通常来自 NextSequence)并在 ZCPacket.Sequence 中携带它。
// 把序号作为显式参数而非内部自增,是为了让发送方在分片或多路径场景下能精确
// 控制到达顺序与序号编码。
//
// **并发约束**:NextSequence 与 Encrypt 是两次独立的加锁操作。当多个
// goroutine 并发地"取号 + 加密"时,两个阶段可能交错,导致先取到的号后加密,
// 从而触发 nonce 重用的防御性拒绝。因此:
//
//   - 单 goroutine 发送路径:NextSequence + Encrypt 是正确的用法;
//   - 多 goroutine 并发发送:必须使用 SealNext,它在同一临界区内完成
//     "取号 + 加密",并直接返回可写入 ZCPacket.Sequence 的序号。
//
// nonce 构造:SessionID(4 字节大端) || 序号(8 字节大端),共 12 字节。
// 这与 AGENTS.md 8.7 的"由 SessionID 与 Sequence 组合"一致,且保证同一会话内
// 不会重用 nonce——不同会话的 SessionID 不同,同一会话的序号单调递增。
func (s *Session) Encrypt(seq uint64, plaintext []byte, additionalData []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session: encrypt %d: %w", s.ID, ErrSessionClosed)
	}
	// nonce 重用的防线:任何严格小于高水位的序号都已加密过,必须拒绝。
	// 高水位本身(seq == lastSent)是下一个可用序号,允许使用。
	if s.lastSent > 0 && seq < s.lastSent {
		return nil, fmt.Errorf("session: encrypt %d: seq %d already used (last %d): %w",
			s.ID, seq, s.lastSent-1, ErrReplay)
	}
	// 推进高水位。用 max 而非直接赋值,以容忍调用方乱序加密。
	if seq+1 > s.lastSent {
		s.lastSent = seq + 1
	}
	// 若调用方绕过了 NextSequence 直接指定了更大的序号,同步推进 sendSeq,
	// 避免之后的 NextSequence 返回一个已被加密过的序号。
	if seq >= s.sendSeq {
		s.sendSeq = seq + 1
	}

	nonce := s.nonce(seq)
	out := s.sendAEAD.Seal(nil, nonce, plaintext, additionalData)
	s.lastUsed = time.Now()
	return out, nil
}

// SealNext 在同一临界区内分配序号并加密,返回序号与密文。
//
// 这是**并发发送场景下唯一正确的用法**。把"取号"与"加密"合并为一个原子
// 操作,消除了两者交错导致的 nonce 重用风险(先取大号、后取小号并发加密时,
// 小号会被高水位防线拒绝,表现为随机丢包)。
//
// 返回的 seq 应写入 ZCPacket.Sequence,使接收端能用相同的 nonce 解开。
func (s *Session) SealNext(plaintext []byte, additionalData []byte) (uint64, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, nil, fmt.Errorf("session: seal next %d: %w", s.ID, ErrSessionClosed)
	}
	if s.sendSeq >= DefaultMaxSendSeq {
		return 0, nil, fmt.Errorf("session: seal next %d: %w", s.ID, ErrSeqExhausted)
	}

	seq := s.sendSeq
	s.sendSeq++
	if seq+1 > s.lastSent {
		s.lastSent = seq + 1
	}

	nonce := s.nonce(seq)
	out := s.sendAEAD.Seal(nil, nonce, plaintext, additionalData)
	s.lastUsed = time.Now()
	return seq, out, nil
}

// Decrypt 使用指定序号解密密文。
//
// 流程严格遵循"先验证、后提交"顺序:
//  1. 检查序号是否可接受(Check,不改状态);
//  2. 执行 AEAD 解密与认证;
//  3. 认证通过后才提交序号(Advance)。
//
// 若顺序颠倒,一个伪造的包就能永久占用一个合法序号,使真实的重传被误判为
// 重放而丢弃——这会让攻击者仅凭发送垃圾包就能造成持续丢包。
func (s *Session) Decrypt(seq uint64, ciphertext []byte, additionalData []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, fmt.Errorf("session: decrypt %d: %w", s.ID, ErrSessionClosed)
	}
	if len(ciphertext) < TagSize {
		return nil, fmt.Errorf("session: decrypt %d: %d bytes: %w (min %d)",
			s.ID, len(ciphertext), ErrShortPacket, TagSize)
	}

	if !s.recvWindow.checkLocked(seq) {
		return nil, fmt.Errorf("session: decrypt %d: seq %d: %w", s.ID, seq, ErrReplay)
	}

	nonce := s.nonce(seq)
	plaintext, err := s.recvAEAD.Open(nil, nonce, ciphertext, additionalData)
	if err != nil {
		// 认证失败:不推进窗口,该序号仍可被合法的重传使用。
		return nil, fmt.Errorf("session: decrypt %d: seq %d: %w: %w", s.ID, seq, ErrDecrypt, err)
	}

	s.recvWindow.Advance(seq)
	s.lastUsed = time.Now()
	return plaintext, nil
}

// nonce 由会话 ID 与序号构造 12 字节 nonce。
//
// 布局:SessionID(4B,大端) || Sequence(8B,大端)。
// 低位放序号、高位放会话 ID,使同一会话内序号连续递增时 nonce 也连续递增,
// 便于调试时肉眼比对。
func (s *Session) nonce(seq uint64) []byte {
	var n [NonceSize]byte
	binary.BigEndian.PutUint32(n[0:4], s.ID)
	binary.BigEndian.PutUint64(n[4:12], seq)
	return n[:]
}

// Expired 报告会话是否已超过空闲存活时间或已被关闭。
func (s *Session) Expired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return true
	}
	return time.Since(s.lastUsed) > s.ttl
}

// Idle 返回自最后一次收发以来的时长。
func (s *Session) Idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed)
}

// Touch 刷新会话的最后活动时间。
//
// 用于接收方向没有可解密数据但连接仍然活跃的场景(例如仅收到保活包),
// 避免活跃会话被 GC 误回收。
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
}

// Close 关闭会话,之后所有加解密都会返回 ErrSessionClosed。
//
// 同时清零密钥,缩短密钥在内存中的驻留时间。
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true

	// 清零密钥。这不是完备的"内存擦除"(GC 可能已复制过这些字节),
	// 但能覆盖最常见的残留场景。
	for i := range s.sendKey {
		s.sendKey[i] = 0
	}
	for i := range s.recvKey {
		s.recvKey[i] = 0
	}
	s.sendAEAD = nil
	s.recvAEAD = nil
	s.recvWindow.Reset()
}

// Closed 报告会话是否已关闭。
func (s *Session) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// RecvWindow 返回接收方向的抗重放窗口。
func (s *Session) RecvWindow() *ReplayWindow { return s.recvWindow }

// SetTTL 设置会话的空闲存活时间。
func (s *Session) SetTTL(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl > 0 {
		s.ttl = ttl
	}
}

// TTL 返回会话的空闲存活时间。
func (s *Session) TTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// NewSessionID 生成一个随机的会话标识。
//
// 随机而非自增:会话 ID 参与 nonce 构造,若两端的 ID 相同且序号空间重叠,
// 同一条链路上会存在 nonce 重用风险。随机化把碰撞概率降到可忽略。
func NewSessionID() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("session: generate session id: %w", err)
	}
	id := binary.BigEndian.Uint32(b[:])
	// 0 保留为"未指定",避免与零值 ZCPacket 混淆。
	if id == 0 {
		id = 1
	}
	return id, nil
}

// DeriveSessionID 由两端公钥确定性地导出会话 ID。
//
// 确定性导出的好处:双向同时发起握手时,两端会算出同一个 ID,从而能识别出
// "这是同一个会话的两条握手路径"并去重,而不是建立两条互不相识的会话。
// 用 XOR 保证结果与发起方/响应方的角色无关。
func DeriveSessionID(pubA, pubB [KeySize]byte) uint32 {
	var mixed [KeySize]byte
	for i := range mixed {
		mixed[i] = pubA[i] ^ pubB[i]
	}

	sum := sha256.Sum256(mixed[:])
	id := binary.BigEndian.Uint32(sum[:4])
	if id == 0 {
		id = 1
	}
	return id
}
