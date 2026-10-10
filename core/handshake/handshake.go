package handshake

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/flynn/noise"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/session"
)

// HandshakeTimeout 是单次握手的超时。
//
// 与 AGENTS.md 8.11 的 HandshakeTimeout 常量一致。
const HandshakeTimeout = 5 * time.Second

// HandshakeMaxRetry 是握手的最大重试次数(AGENTS.md 8.11),与超时共同界定
// "重试耗尽后转入中继协商"的判定(AGENTS.md 8.5)。
const HandshakeMaxRetry = 5

// 握手阶段。
const (
	// StageInit 表示尚未处理任何消息。
	StageInit = iota
	// StageSentInit 表示发起方已发出 Init。
	StageSentInit
	// StageSentResp 表示响应方已发出 Resp。
	StageSentResp
	// StageEstablished 表示握手完成,密钥已派生。
	StageEstablished
)

// 握手相关错误。
var (
	// ErrBadStage 表示当前阶段不接受该操作。
	ErrBadStage = errors.New("handshake: operation invalid in current stage")
	// ErrNoise 表示 Noise 层失败(通常是密钥不匹配)。
	ErrNoise = errors.New("handshake: noise protocol failure")
	// ErrTimeout 表示握手超时。
	ErrTimeout = errors.New("handshake: timed out")
	// ErrPeerNodeID 表示对端身份与预期不符。
	ErrPeerNodeID = errors.New("handshake: peer node id mismatch")
	// ErrRoleMismatch 表示发起方收到了只有响应方能处理的消息(或反之)。
	ErrRoleMismatch = errors.New("handshake: role mismatch")
)

// noiseSuite 是握手使用的密码套件。
//
// Noise_IK_25519_ChaChaPoly_BLAKE2s —— 与 AGENTS.md 8.5 及 7 章技术选型一致。
var noiseSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

// Config 是构造握手的参数。
type Config struct {
	// Local 是本端长期密钥。
	Local *crypto.KeyPair
	// RemotePub 是对端长期公钥。发起方必须提供;响应方在握手完成前未知,
	// 由 IK 模式从消息中解出。
	RemotePub *[crypto.KeySize]byte
	// Initiator 为真表示本端发起握手。
	Initiator bool
	// Prologue 是参与握手哈希的附加数据。
	//
	// 这里绑定网络名称,使不同网络的节点即使持有同一对密钥也无法互通
	// ——相当于一道额外的网络隔离,且不增加任何往返。
	Prologue []byte
	// Timeout 覆盖默认握手超时。
	Timeout time.Duration
}

// Handshake 是一次进行中的 Noise IK 握手。
//
// 状态转换是单向的:Init → SentInit/SentResp → Established。
// 一旦 Established,密钥与会话即可通过 Session 取得。
//
// 并发安全:内部状态由互斥锁保护,同时只允许一个消息处理流程推进。
type Handshake struct {
	mu sync.Mutex

	local     *crypto.KeyPair
	initiator bool
	prologue  []byte
	timeout   time.Duration
	deadline  time.Time

	// noiseState 是 Noise 握手状态机。
	noiseState *noise.HandshakeState

	// stage 是当前阶段。
	stage int

	// senderIndex 是本端为本次会话分配的索引。
	senderIndex uint32
	// receiverIndex 是对端分配的索引。
	receiverIndex uint32

	// remotePub 是对端长期公钥。
	remotePub [crypto.KeySize]byte
	// remoteNodeID 是对端 NodeID,握手完成后由公钥派生。
	remoteNodeID [crypto.NodeIDSize]byte

	// sendCipher / recvCipher 是握手派生出的两个方向的 CipherState。
	sendCipher *noise.CipherState
	recvCipher *noise.CipherState

	// session 是握手完成后构造的加密会话。
	session *session.Session

	// timestampSeen 记录已接受的时间戳,用于拒绝重放。
	timestampSeen map[uint64]time.Time

	// lastError 记录失败原因,便于上层诊断。
	lastError error

	// establishedAt 记录握手完成时间。
	establishedAt time.Time
}

// NewInitiator 构造握手发起方。
//
// 发起方必须预先知道响应方的公钥 —— 这正是 IK 模式的定义:发起方在第一条
// 消息中就用 "es" 与 "s" 令牌加密自己的静态公钥,因此需要先知道对端公钥。
func NewInitiator(local *crypto.KeyPair, remotePub [crypto.KeySize]byte) (*Handshake, error) {
	return newHandshake(Config{
		Local:     local,
		RemotePub: &remotePub,
		Initiator: true,
	})
}

// NewResponder 构造握手响应方。
//
// 响应方不需要预知发起方公钥:IK 模式会从第一条消息的密文中解出它。
func NewResponder(local *crypto.KeyPair) (*Handshake, error) {
	return newHandshake(Config{
		Local:     local,
		Initiator: false,
	})
}

// NewWithConfig 使用完整配置构造握手。
func NewWithConfig(cfg Config) (*Handshake, error) {
	return newHandshake(cfg)
}

// newHandshake 是构造握手实例的内部实现。
func newHandshake(cfg Config) (*Handshake, error) {
	if cfg.Local == nil {
		return nil, fmt.Errorf("handshake: new: local key pair must not be nil")
	}
	if cfg.Initiator && cfg.RemotePub == nil {
		return nil, fmt.Errorf("handshake: new: initiator requires remote public key")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = HandshakeTimeout
	}

	// 构造 Noise 配置。StaticKeypair 是本端的长期密钥。
	noiseCfg := noise.Config{
		CipherSuite:   noiseSuite,
		Pattern:       noise.HandshakeIK,
		Initiator:     cfg.Initiator,
		StaticKeypair: noise.DHKey{Private: cfg.Local.PrivateKey[:], Public: cfg.Local.PublicKey[:]},
		Prologue:      cfg.Prologue,
	}

	if cfg.Initiator {
		// IK 模式中发起方把响应方公钥作为 pre-message 提供。
		noiseCfg.PeerStatic = cfg.RemotePub[:]
	}

	ns, err := noise.NewHandshakeState(noiseCfg)
	if err != nil {
		return nil, fmt.Errorf("handshake: new: %w: %w", ErrNoise, err)
	}

	senderIndex, err := newSenderIndex()
	if err != nil {
		return nil, err
	}

	h := &Handshake{
		local:         cfg.Local,
		initiator:     cfg.Initiator,
		prologue:      cfg.Prologue,
		timeout:       timeout,
		deadline:      time.Now().Add(timeout),
		noiseState:    ns,
		stage:         StageInit,
		senderIndex:   senderIndex,
		timestampSeen: make(map[uint64]time.Time),
	}

	// 发起方预知对端公钥,可以直接记录身份。
	if cfg.RemotePub != nil {
		h.remotePub = *cfg.RemotePub
		h.remoteNodeID = crypto.NodeIDFromPublicKey(*cfg.RemotePub)
	}

	return h, nil
}

// Initiator 报告本端是否为发起方。
func (h *Handshake) Initiator() bool { return h.initiator }

// Stage 返回当前握手阶段。
func (h *Handshake) Stage() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stage
}

// SenderIndex 返回本端分配的会话索引。
func (h *Handshake) SenderIndex() uint32 { return h.senderIndex }

// ReceiverIndex 返回对端分配的会话索引。
func (h *Handshake) ReceiverIndex() uint32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.receiverIndex
}

// RemotePublicKey 返回对端长期公钥。
func (h *Handshake) RemotePublicKey() [crypto.KeySize]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.remotePub
}

// RemoteNodeID 返回对端 NodeID,由握手得到的公钥派生。
func (h *Handshake) RemoteNodeID() [crypto.NodeIDSize]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.remoteNodeID
}

// LocalNodeID 返回本端 NodeID。
func (h *Handshake) LocalNodeID() [crypto.NodeIDSize]byte {
	return h.local.NodeID()
}

// LastError 返回最近一次失败原因。
func (h *Handshake) LastError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastError
}

// Expired 报告握手是否已超过截止时间。
func (h *Handshake) Expired() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Now().After(h.deadline)
}

// checkDeadline 校验是否超时。
func (h *Handshake) checkDeadline() error {
	if time.Now().After(h.deadline) {
		h.lastError = ErrTimeout
		return fmt.Errorf("handshake: %w after %v", ErrTimeout, h.timeout)
	}
	return nil
}

// WriteMessage 生成下一条要发送的握手消息。
//
// 返回的消息是**完整的线格式**(含头、时间戳与 MAC),可直接写入传输层。
// 发起方首次调用产出 Init,收到 Resp 后再次调用产出 Complete;
// 响应方收到 Init 后调用产出 Resp。
func (h *Handshake) WriteMessage() ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.checkDeadline(); err != nil {
		return nil, err
	}
	if h.stage == StageEstablished {
		return nil, fmt.Errorf("handshake: write message: %w", ErrComplete)
	}

	// mac1Key 用对端公钥派生:接收方能用自己公钥独立复算并校验。
	var mac1Key []byte
	if !isZero(h.remotePub[:]) {
		mac1Key = DeriveMAC1Key(h.remotePub[:])
	}

	switch {
	case h.initiator && h.stage == StageInit:
		// 第一条消息:Noise IK 的 message 1。
		payload, _, _, err := h.noiseState.WriteMessage(nil, nil)
		if err != nil {
			h.lastError = err
			return nil, fmt.Errorf("handshake: write init: %w: %w", ErrNoise, err)
		}

		h.stage = StageSentInit
		ts := nowNanos()
		h.timestampSeen[ts] = time.Now()

		return EncodeInit(h.senderIndex, ts, payload, mac1Key, nil), nil

	case h.initiator && h.stage == StageSentInit:
		// 第三条:应用层确认,Noise 已无消息可写。
		h.stage = StageEstablished
		h.establishedAt = time.Now()
		return EncodeComplete(h.senderIndex, h.receiverIndex, mac1Key), nil

	case !h.initiator && h.stage == StageInit:
		// 响应方在 ReadMessage 之后才写 Resp,此处交由 ReadMessage 处理。
		return nil, fmt.Errorf("handshake: write message: responder must read init first: %w", ErrBadStage)

	case !h.initiator && h.stage == StageSentInit:
		// 已处理过 Init,产出 Resp。
		// 这是 IK 的最后一条 Noise 消息,本次调用会返回派生完成的密钥。
		payload, send, recv, err := h.noiseState.WriteMessage(nil, nil)
		if err != nil {
			h.lastError = err
			return nil, fmt.Errorf("handshake: write resp: %w: %w", ErrNoise, err)
		}

		// 发出 message 2 后 Noise 完成模式并 Split(),本次调用即返回密钥。
		if err := h.adoptCiphers(send, recv); err != nil {
			return nil, err
		}

		h.stage = StageEstablished
		h.establishedAt = time.Now()

		return EncodeResp(h.senderIndex, h.receiverIndex, payload, mac1Key, nil), nil
	}

	return nil, fmt.Errorf("handshake: write message at stage %d: %w", h.stage, ErrBadStage)
}

// ReadMessage 处理一条收到的握手消息,返回握手载荷(若有)。
//
// 消息必须先通过长度、类型、保留字段、时间戳与 MAC1 校验,再交给 Noise
// ——昂贵运算只在校验通过后发生,这是抗 DoS 的核心(AGENTS.md 8.5)。
func (h *Handshake) ReadMessage(msg []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.checkDeadline(); err != nil {
		return nil, err
	}

	msgType, err := MessageType(msg)
	if err != nil {
		return nil, err
	}

	switch msgType {
	case MsgTypeInit:
		return h.readInit(msg)
	case MsgTypeResp:
		return h.readResp(msg)
	case MsgTypeComplete:
		return h.readComplete(msg)
	default:
		return nil, fmt.Errorf("handshake: read message: type %d: %w", msgType, ErrBadMessageType)
	}
}

// verifyInboundMAC1 校验入站消息的 MAC1。
func (h *Handshake) verifyInboundMAC1(msg []byte) error {
	// MAC1 以**接收方的公钥**派生,即本端自己的公钥。
	key := DeriveMAC1Key(h.local.PublicKey[:])
	if err := VerifyMAC1(key, msg); err != nil {
		h.lastError = err
		return err
	}
	return nil
}

// readInit 处理握手发起消息。
func (h *Handshake) readInit(msg []byte) ([]byte, error) {
	if h.initiator {
		// 发起方不应处理 Init;这通常意味着双方同时发起了握手,
		// 上层应通过角色比较(如 NodeID 大小)决定谁退让。
		return nil, fmt.Errorf("handshake: initiator received init: %w", ErrRoleMismatch)
	}
	if h.stage != StageInit {
		return nil, fmt.Errorf("handshake: read init at stage %d: %w", h.stage, ErrBadStage)
	}

	parsed, err := ParseInit(msg)
	if err != nil {
		h.lastError = err
		return nil, err
	}

	if err := h.verifyInboundMAC1(msg); err != nil {
		return nil, err
	}
	if err := VerifyMAC2(nil, msg); err != nil {
		return nil, err
	}

	// 时间戳校验:窗口 + 重放双重防护。
	if err := CheckTimestamp(parsed.Timestamp, nowNanos()); err != nil {
		h.lastError = err
		return nil, err
	}
	if _, seen := h.timestampSeen[parsed.Timestamp]; seen {
		h.lastError = ErrTimestampReplay
		return nil, fmt.Errorf("handshake: timestamp %d: %w", parsed.Timestamp, ErrTimestampReplay)
	}
	h.timestampSeen[parsed.Timestamp] = time.Now()

	h.receiverIndex = parsed.SenderIndex

	// 交给 Noise 处理。此时才会做曲线运算。
	payload, send, recv, err := h.noiseState.ReadMessage(nil, parsed.Payload)
	if err != nil {
		h.lastError = err
		return nil, fmt.Errorf("handshake: read init payload: %w: %w", ErrNoise, err)
	}

	// Noise 已解出对端静态公钥,据此确定身份。
	peerStatic := h.noiseState.PeerStatic()
	if len(peerStatic) != crypto.KeySize {
		return nil, fmt.Errorf("handshake: peer static key length %d: %w", len(peerStatic), ErrNoise)
	}
	copy(h.remotePub[:], peerStatic)
	h.remoteNodeID = crypto.NodeIDFromPublicKey(h.remotePub)

	// 此处不采纳密钥。IK 模式有两条 Noise 消息,响应方读完 message 1 时
	// msgIdx 仍为 1 < 2,Noise 尚未 Split(),返回的 cipher state 是 nil。
	// 密钥要等到 WriteMessage 发出 Resp(message 2)时才产生。
	//
	// 这个顺序很容易写错:把采纳点放在 readInit 会得到 nil 密钥,
	// 而错误信息只会说"noise protocol failure",很难指向真正的原因。
	_ = send
	_ = recv

	// 记录阶段,允许 WriteMessage 产出 Resp。
	h.stage = StageSentInit

	return payload, nil
}

// readResp 处理握手响应消息。
func (h *Handshake) readResp(msg []byte) ([]byte, error) {
	if !h.initiator {
		return nil, fmt.Errorf("handshake: responder received resp: %w", ErrRoleMismatch)
	}
	if h.stage != StageSentInit {
		return nil, fmt.Errorf("handshake: read resp at stage %d: %w", h.stage, ErrBadStage)
	}

	parsed, err := ParseResp(msg)
	if err != nil {
		h.lastError = err
		return nil, err
	}

	if err := h.verifyInboundMAC1(msg); err != nil {
		return nil, err
	}
	if err := VerifyMAC2(nil, msg); err != nil {
		return nil, err
	}

	if parsed.ReceiverIndex != h.senderIndex {
		return nil, fmt.Errorf("handshake: resp receiver index %d != our sender index %d: %w",
			parsed.ReceiverIndex, h.senderIndex, ErrBadStage)
	}
	h.receiverIndex = parsed.SenderIndex

	payload, send, recv, err := h.noiseState.ReadMessage(nil, parsed.Payload)
	if err != nil {
		h.lastError = err
		return nil, fmt.Errorf("handshake: read resp payload: %w: %w", ErrNoise, err)
	}

	// 发起方处理完 Resp 即完成整个 Noise 模式,密钥在此返回。
	// send 用于本端→对端,recv 用于对端→本端。
	if err := h.adoptCiphers(send, recv); err != nil {
		return nil, err
	}

	// 校验对端身份与预期一致 —— 防止中间人替换公钥。
	peerStatic := h.noiseState.PeerStatic()
	if len(peerStatic) == crypto.KeySize && !isZero(h.remotePub[:]) {
		var got [crypto.KeySize]byte
		copy(got[:], peerStatic)
		if got != h.remotePub {
			h.lastError = ErrPeerNodeID
			return nil, fmt.Errorf("handshake: peer static key mismatch: %w", ErrPeerNodeID)
		}
	}

	return payload, nil
}

// readComplete 处理握手确认消息。
func (h *Handshake) readComplete(msg []byte) ([]byte, error) {
	if h.stage != StageEstablished && h.stage != StageSentInit {
		return nil, fmt.Errorf("handshake: read complete at stage %d: %w", h.stage, ErrBadStage)
	}

	senderIdx, receiverIdx, payload, err := ParseComplete(msg)
	if err != nil {
		h.lastError = err
		return nil, err
	}

	if err := h.verifyInboundMAC1(msg); err != nil {
		return nil, err
	}

	if receiverIdx != h.senderIndex {
		return nil, fmt.Errorf("handshake: complete receiver index %d != our sender index %d: %w",
			receiverIdx, h.senderIndex, ErrBadStage)
	}
	if h.receiverIndex == 0 {
		h.receiverIndex = senderIdx
	}

	h.stage = StageEstablished
	if h.establishedAt.IsZero() {
		h.establishedAt = time.Now()
	}
	return payload, nil
}

// adoptCiphers 采纳 Noise 派生出的双向密钥并构造会话。
//
// **方向映射是这里唯一容易出错的地方,必须严格按角色区分。**
//
// flynn/noise 的 Split() 返回 (c1, c2),其中 c1 恒为**发起方发送**方向的
// 密钥、c2 恒为**响应方发送**方向的密钥。这个顺序与角色无关:两端算出的
// (c1, c2) 完全相同,不做镜像交换。实测确认:
//
//	A.send == B.send   (都等于 c1)
//	A.recv == B.recv   (都等于 c2)
//
// 因此必须按本端角色决定谁用哪一个:
//
//	发起方:send=c1, recv=c2
//	响应方:send=c2, recv=c1
//
// 若照搬"直接使用返回值"的写法,两端会用同一把密钥加密、另一把解密,
// 结果是双方都解不开对方的包。这个错误不会在握手中暴露——握手本身是成功的
// ——只在第一次收发数据时才表现为"解密失败",排查成本很高。
func (h *Handshake) adoptCiphers(c1, c2 *noise.CipherState) error {
	if c1 == nil || c2 == nil {
		return fmt.Errorf("handshake: adopt ciphers: %w (nil cipher state)", ErrNoise)
	}

	var send, recv *noise.CipherState
	if h.initiator {
		send, recv = c1, c2
	} else {
		send, recv = c2, c1
	}

	h.sendCipher = send
	h.recvCipher = recv

	// 会话 ID 由两端公钥确定性导出:双向同时发起时两端会算出同一 ID,
	// 从而能识别出这是同一会话的两条路径而非两个会话。
	id := session.DeriveSessionID(h.local.PublicKey, h.remotePub)

	s, err := session.NewSession(id, send.UnsafeKey(), recv.UnsafeKey())
	if err != nil {
		return fmt.Errorf("handshake: adopt ciphers: build session: %w", err)
	}
	s.PeerID = h.remoteNodeID
	h.session = s

	return nil
}

// Complete 报告握手是否已完成。
func (h *Handshake) Complete() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stage == StageEstablished && h.session != nil
}

// Session 返回握手建立的加密会话。
func (h *Handshake) Session() (*session.Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.session == nil {
		return nil, fmt.Errorf("handshake: session: %w", ErrIncomplete)
	}
	return h.session, nil
}

// PeerNodeIDMatches 报告握手得到的对端身份是否与预期一致。
func (h *Handshake) PeerNodeIDMatches(expected [crypto.NodeIDSize]byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.remoteNodeID == expected
}

// Close 释放握手状态与派生的临时密钥。
func (h *Handshake) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.session != nil {
		h.session.Close()
		h.session = nil
	}
	h.sendCipher = nil
	h.recvCipher = nil
	h.noiseState = nil
	h.stage = StageEstablished
}

// Fingerprint 返回对端公钥的短指纹,用于日志。
func (h *Handshake) Fingerprint() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	sum := sha256.Sum256(h.remotePub[:])
	return fmt.Sprintf("%x", sum[:8])
}

// isZero 判断字节切片是否全零。
func isZero(b []byte) bool {
	var acc byte
	for _, v := range b {
		acc |= v
	}
	return acc == 0
}
