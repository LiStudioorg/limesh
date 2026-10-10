// Package handshake 实现 Limesh 的 Noise IK 握手(1-RTT)。
//
// 握手完成两件事:
//  1. 通过 Noise IK 双向认证并派生会话密钥;
//  2. 把密钥交给 core/session 构造加解密会话。
//
// 设计说明见 AGENTS.md 8.5。身份与密钥持久化见 core/crypto。
package handshake

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// 线格式常量。
//
// 握手消息 = 固定头 + Noise 载荷,布局与 WireGuard 的握手包同构:
//
//	偏移  长度  字段
//	 0     1    Type
//	 1     3    Reserved(必须为 0,为将来扩展预留)
//	 4     4    SenderIndex
//	 8    12    Timestamp(Unix 纳秒,仅 Init 消息)
//	...    N    Noise 载荷(变长)
//	...   16    MAC1
//	...   16    MAC2
//
// 选 12 字节时间戳而非 8 字节:与 Noise 载荷里常见的 nonce 尺寸对齐,
// 且纳秒精度在 12 字节内可表示到公元 10889 年,不存在溢出顾虑。
const (
	// MsgTypeInit 是握手发起消息。
	MsgTypeInit byte = 1
	// MsgTypeResp 是握手响应消息。
	MsgTypeResp byte = 2
	// MsgTypeComplete 是握手确认消息(应用层确认,非 Noise 消息)。
	MsgTypeComplete byte = 3

	// headerSize 是 Init/Resp 消息固定头长度:Type(1) + Reserved(3) + SenderIndex(4)。
	headerSize = 8
	// timestampSize 是时间戳字段长度。
	timestampSize = 12
	// macSize 是单个 MAC 字段长度。
	macSize = 16
	// macKeyLabel 是派生 MAC 密钥使用的标签。
	macKeyLabel = "limesh-mac1-v1"
	// cookieKeyLabel 是派生 cookie 密钥使用的标签。
	cookieKeyLabel = "limesh-cookie-v1"

	// MAC1OffsetFromEnd 是 MAC1 相对消息末尾的偏移。
	MAC1OffsetFromEnd = 2 * macSize
	// MAC2OffsetFromEnd 是 MAC2 相对消息末尾的偏移。
	MAC2OffsetFromEnd = macSize

	// MinInitSize 是 Init 消息的最小长度(空 Noise 载荷)。
	MinInitSize = headerSize + timestampSize + 2*macSize
	// MinRespSize 是 Resp 消息的最小长度。
	MinRespSize = headerSize + 2*macSize
)

// 握手相关错误。
var (
	// ErrBadMessageType 表示消息类型不合法。
	ErrBadMessageType = errors.New("handshake: bad message type")
	// ErrShortMessage 表示消息长度不足。
	ErrShortMessage = errors.New("handshake: message too short")
	// ErrMAC1 表示 MAC1 校验失败。
	ErrMAC1 = errors.New("handshake: mac1 verification failed")
	// ErrMAC2 表示 MAC2 校验失败。
	ErrMAC2 = errors.New("handshake: mac2 verification failed")
	// ErrTimestampSkew 表示时间戳超出允许偏差。
	ErrTimestampSkew = errors.New("handshake: timestamp outside acceptable window")
	// ErrTimestampReplay 表示时间戳重复或过旧。
	ErrTimestampReplay = errors.New("handshake: timestamp replay detected")
	// ErrReservedNotZero 表示保留字段非零。
	ErrReservedNotZero = errors.New("handshake: reserved field must be zero")
	// ErrComplete 表示握手状态不允许该操作。
	ErrComplete = errors.New("handshake: already complete")
	// ErrIncomplete 表示握手尚未完成。
	ErrIncomplete = errors.New("handshake: not complete")
)

// InitMessage 是握手发起消息的解析结果。
type InitMessage struct {
	// SenderIndex 是发起方为本次会话分配的索引。
	SenderIndex uint32
	// Timestamp 是发起方发送时的 Unix 纳秒时间戳。
	Timestamp uint64
	// Payload 是 Noise 载荷(不含头、时间戳与 MAC)。
	Payload []byte
}

// RespMessage 是握手响应消息的解析结果。
type RespMessage struct {
	// SenderIndex 是响应方为本次会话分配的索引。
	SenderIndex uint32
	// ReceiverIndex 回显 Init 的 SenderIndex,用于双方把索引关联起来。
	ReceiverIndex uint32
	// Payload 是 Noise 载荷。
	Payload []byte
}

// EncodeInit 编码一条握手发起消息。
//
// mac1Key 用于计算 MAC1(取接收方公钥派生),cookie 为空时不写入有效 MAC2
// ——Phase 1a 只保留该字段位置,不实现 cookie 逻辑(AGENTS.md 8.5 的"抗 DoS"
// 完整实现留到后续阶段)。
func EncodeInit(senderIndex uint32, timestamp uint64, payload []byte, mac1Key []byte, cookie []byte) []byte {
	msg := make([]byte, 0, headerSize+timestampSize+len(payload)+2*macSize)

	msg = append(msg, MsgTypeInit, 0, 0, 0)

	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], senderIndex)
	msg = append(msg, idx[:]...)

	var ts [timestampSize]byte
	binary.BigEndian.PutUint64(ts[timestampSize-8:], timestamp)
	msg = append(msg, ts[:]...)

	msg = append(msg, payload...)

	return appendMACs(msg, mac1Key, cookie)
}

// EncodeResp 编码一条握手响应消息。
func EncodeResp(senderIndex, receiverIndex uint32, payload []byte, mac1Key []byte, cookie []byte) []byte {
	msg := make([]byte, 0, headerSize+4+len(payload)+2*macSize)

	msg = append(msg, MsgTypeResp, 0, 0, 0)

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], senderIndex)
	msg = append(msg, buf[:]...)
	binary.BigEndian.PutUint32(buf[:], receiverIndex)
	msg = append(msg, buf[:]...)

	msg = append(msg, payload...)

	return appendMACs(msg, mac1Key, cookie)
}

// EncodeComplete 编码握手的应用层确认消息。
//
// Noise IK 本身只有两条消息;这条确认用于让发起方明确知道"响应方也已就绪",
// 从而把状态机从 Handshaking 推进到 Direct(AGENTS.md 8.9.1)。
func EncodeComplete(senderIndex, receiverIndex uint32, mac1Key []byte) []byte {
	msg := make([]byte, 0, headerSize+4+2*macSize)

	msg = append(msg, MsgTypeComplete, 0, 0, 0)

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], senderIndex)
	msg = append(msg, buf[:]...)
	binary.BigEndian.PutUint32(buf[:], receiverIndex)
	msg = append(msg, buf[:]...)

	return appendMACs(msg, mac1Key, nil)
}

// appendMACs 在消息尾部追加 MAC1 与 MAC2 字段并返回完整消息。
func appendMACs(msg []byte, mac1Key, cookie []byte) []byte {
	mac1 := computeMAC(mac1Key, msg)

	out := make([]byte, 0, len(msg)+2*macSize)
	out = append(out, msg...)
	out = append(out, mac1...)

	// MAC2 覆盖 [消息 | MAC1]。cookie 为空时写零值占位。
	mac2 := computeMAC(cookie, out)
	out = append(out, mac2...)
	return out
}

// computeMAC 计算 16 字节截断 HMAC-SHA256。
//
// key 为空时返回全零 —— 对应"cookie 尚未协商"的状态,校验方据此跳过
// MAC2 检查。这样 Phase 1a 可以在不实现 cookie 机制的前提下保留字段与
// 校验路径,后续阶段只需让 key 非空即可自动启用。
func computeMAC(key, data []byte) []byte {
	out := make([]byte, macSize)
	if len(key) == 0 {
		return out
	}

	h := hmac.New(sha256.New, key)
	h.Write(data)
	sum := h.Sum(nil)
	copy(out, sum[:macSize])
	return out
}

// VerifyMAC1 校验消息的 MAC1 字段。
func VerifyMAC1(key, msg []byte) error {
	if len(msg) < 2*macSize {
		return fmt.Errorf("handshake: verify mac1: %d bytes: %w", len(msg), ErrShortMessage)
	}

	body := msg[:len(msg)-MAC1OffsetFromEnd]
	got := msg[len(msg)-MAC1OffsetFromEnd : len(msg)-MAC2OffsetFromEnd]
	want := computeMAC(key, body)

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMAC1
	}
	return nil
}

// VerifyMAC2 校验消息的 MAC2 字段。
//
// cookie 为空时返回 nil:表示本端尚未签发 cookie,不做检查。
func VerifyMAC2(cookie, msg []byte) error {
	if len(cookie) == 0 {
		return nil
	}
	if len(msg) < 2*macSize {
		return fmt.Errorf("handshake: verify mac2: %d bytes: %w", len(msg), ErrShortMessage)
	}

	body := msg[:len(msg)-MAC2OffsetFromEnd]
	got := msg[len(msg)-MAC2OffsetFromEnd:]
	want := computeMAC(cookie, body)

	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMAC2
	}
	return nil
}

// DeriveMAC1Key 由公钥派生 MAC1 密钥。
//
// MAC1 的作用是"证明发送方知道接收方公钥",使未持有正确公钥的扫描流量
// 无法激起昂贵的握手运算(AGENTS.md 8.5 的抗 DoS 要求)。
func DeriveMAC1Key(pub []byte) []byte {
	h := sha256.New()
	h.Write([]byte(macKeyLabel))
	h.Write(pub)
	return h.Sum(nil)
}

// DeriveCookieKey 由公钥与来源地址派生 cookie 密钥。
//
// 绑定来源地址使 cookie 无法被重放到其它地址,这正是抗 DoS 的意义所在。
func DeriveCookieKey(pub []byte, remoteAddr string) []byte {
	h := sha256.New()
	h.Write([]byte(cookieKeyLabel))
	h.Write(pub)
	h.Write([]byte(remoteAddr))
	return h.Sum(nil)
}

// ParseInit 解析握手发起消息。
func ParseInit(msg []byte) (*InitMessage, error) {
	if len(msg) < MinInitSize {
		return nil, fmt.Errorf("handshake: parse init: %d bytes: %w (min %d)",
			len(msg), ErrShortMessage, MinInitSize)
	}
	if msg[0] != MsgTypeInit {
		return nil, fmt.Errorf("handshake: parse init: type %d: %w", msg[0], ErrBadMessageType)
	}
	if err := checkReserved(msg); err != nil {
		return nil, err
	}

	body := msg[:len(msg)-2*macSize]

	return &InitMessage{
		SenderIndex: binary.BigEndian.Uint32(body[4:8]),
		Timestamp:   binary.BigEndian.Uint64(body[headerSize+timestampSize-8 : headerSize+timestampSize]),
		Payload:     body[headerSize+timestampSize:],
	}, nil
}

// ParseResp 解析握手响应消息。
func ParseResp(msg []byte) (*RespMessage, error) {
	if len(msg) < MinRespSize {
		return nil, fmt.Errorf("handshake: parse resp: %d bytes: %w (min %d)",
			len(msg), ErrShortMessage, MinRespSize)
	}
	if msg[0] != MsgTypeResp {
		return nil, fmt.Errorf("handshake: parse resp: type %d: %w", msg[0], ErrBadMessageType)
	}
	if err := checkReserved(msg); err != nil {
		return nil, err
	}

	body := msg[:len(msg)-2*macSize]

	return &RespMessage{
		SenderIndex:   binary.BigEndian.Uint32(body[4:8]),
		ReceiverIndex: binary.BigEndian.Uint32(body[8:12]),
		Payload:       body[12:],
	}, nil
}

// ParseComplete 解析握手确认消息,返回 (senderIndex, receiverIndex, payload)。
func ParseComplete(msg []byte) (uint32, uint32, []byte, error) {
	if len(msg) < MinRespSize {
		return 0, 0, nil, fmt.Errorf("handshake: parse complete: %d bytes: %w (min %d)",
			len(msg), ErrShortMessage, MinRespSize)
	}
	if msg[0] != MsgTypeComplete {
		return 0, 0, nil, fmt.Errorf("handshake: parse complete: type %d: %w", msg[0], ErrBadMessageType)
	}
	if err := checkReserved(msg); err != nil {
		return 0, 0, nil, err
	}

	body := msg[:len(msg)-2*macSize]
	return binary.BigEndian.Uint32(body[4:8]),
		binary.BigEndian.Uint32(body[8:12]),
		body[12:], nil
}

// checkReserved 校验保留字段为零。
//
// 保留字段参与 MAC 计算,因此拒绝非零值可以防止未来版本用这段空间承载
// 新语义时,旧版本误把新消息当作合法消息处理(AGENTS.md 8.10 的降级攻击防护)。
func checkReserved(msg []byte) error {
	if msg[1] != 0 || msg[2] != 0 || msg[3] != 0 {
		return fmt.Errorf("handshake: reserved bytes %v: %w", msg[1:4], ErrReservedNotZero)
	}
	return nil
}

// MessageType 返回握手消息的类型字节。
func MessageType(msg []byte) (byte, error) {
	if len(msg) < 1 {
		return 0, fmt.Errorf("handshake: message type: %w", ErrShortMessage)
	}
	return msg[0], nil
}

// TimestampWindow 是接受的时间戳偏差窗口。
//
// 取 2 分钟:足够容忍常见的时钟漂移与网络延迟,又短到使重放窗口有实际意义。
// 超出该窗口的握手一律拒绝,防止攻击者录制并重放旧握手。
const TimestampWindow = 2 * time.Minute

// nowNanos 返回当前 Unix 纳秒时间戳。抽成变量便于测试注入。
var nowNanos = func() uint64 { return uint64(time.Now().UnixNano()) }

// newSenderIndex 生成随机的会话索引。
func newSenderIndex() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("handshake: generate sender index: %w", err)
	}
	idx := binary.BigEndian.Uint32(b[:])
	if idx == 0 {
		idx = 1
	}
	return idx, nil
}

// CheckTimestamp 校验时间戳是否在允许窗口内。
func CheckTimestamp(ts uint64, now uint64) error {
	// 时间戳是纳秒,窗口换算成纳秒比较。
	window := uint64(TimestampWindow.Nanoseconds())

	if ts > now {
		// 允许一定程度的未来偏差(时钟超前),超出即拒绝。
		if ts-now > window {
			return fmt.Errorf("handshake: timestamp %d is %v ahead: %w",
				ts, time.Duration(ts-now), ErrTimestampSkew)
		}
		return nil
	}
	if now-ts > window {
		return fmt.Errorf("handshake: timestamp %d is %v behind: %w",
			ts, time.Duration(now-ts), ErrTimestampSkew)
	}
	return nil
}
