// Package packet 实现 Limesh 数据面的 ZCPacket 编解码。
//
// ZCPacket 采用 48 字节固定头 + 变长 payload 的线格式,所有多字节字段为
// 网络字节序(大端)。固定头无变长字段,因此可以零拷贝解析,且长度可预测,
// 便于用 sync.Pool 复用缓冲。
//
// 线格式定义见 AGENTS.md 第 8.3 节。
package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// 协议常量,取值见 AGENTS.md 第 8.11 节。
const (
	// Magic 是包头魔数 'L',用于快速丢弃非法包。
	Magic byte = 0x4C
	// ProtoVersion 是当前协议版本。
	ProtoVersion byte = 1
	// HeaderSize 是固定头长度(字节)。
	HeaderSize = 48
	// NodeIDSize 是节点标识长度(字节),为 SHA256(pubkey) 的前 16 字节。
	NodeIDSize = 16
	// MaxPayloadSize 是单个 ZCPacket 允许的最大载荷,防止恶意长度导致巨额分配。
	MaxPayloadSize = 65535
)

// 包类型,取值见 AGENTS.md 第 8.4 节。
const (
	// TypeHandshake 是握手包,承载 Noise IK 消息。
	TypeHandshake byte = iota + 1
	// TypeHandshakeResp 是握手响应包,承载 Noise IK 响应。
	TypeHandshakeResp
	// TypeData 是数据包,载荷为加密 IP 包。
	TypeData
	// TypeKeepalive 是保活包,空体。
	TypeKeepalive
	// TypeLSA 是路由通告包,承载链路状态。
	TypeLSA
	// TypePunch 是打洞包,承载探测载荷。
	TypePunch
	// TypeRelay 是中继包,承载被转发的 ZCPacket。
	TypeRelay
	// TypeProbe 是探测包,承载时间戳。
	TypeProbe
	// TypeClose 是关闭包,承载原因码。
	TypeClose
	// TypeProbeAck 是探测确认包,回显探测时间戳(Phase 1c 引入)。
	TypeProbeAck
)

// Flags 位标志,取值见 AGENTS.md 第 8.3 节。
const (
	// FlagEncrypted 表示载荷已加密。
	FlagEncrypted byte = 1 << 0
	// FlagRelayed 表示该包正被中继转发。
	FlagRelayed byte = 1 << 1
	// FlagProbe 表示这是一个探测包。
	FlagProbe byte = 1 << 2
	// FlagFragStart 表示分片首片。
	FlagFragStart byte = 1 << 3
	// FlagFragEnd 表示分片末片。
	FlagFragEnd byte = 1 << 4
)

// 头内字段偏移量。集中定义避免各处硬编码魔数。
const (
	offMagic     = 0
	offVer       = 1
	offType      = 2
	offFlags     = 3
	offSessionID = 4
	offSequence  = 8
	offAck       = 12
	offSrcNode   = 16
	offDstNode   = 32
)

// 哨兵错误。调用方可用 errors.Is 判定。
var (
	// ErrShortBuffer 表示输入缓冲区不足以容纳一个完整的头或包。
	ErrShortBuffer = errors.New("packet: buffer too short")
	// ErrBadMagic 表示包头魔数不匹配,该包应被静默丢弃。
	ErrBadMagic = errors.New("packet: bad magic")
	// ErrBadVersion 表示协议版本不受支持。
	ErrBadVersion = errors.New("packet: unsupported version")
	// ErrPayloadTooLarge 表示载荷超过 MaxPayloadSize。
	ErrPayloadTooLarge = errors.New("packet: payload too large")
)

// ZCPacket 是一个零拷贝的协议数据单元。
//
// ZCPacket 持有一段 buf,头部占用 buf[:HeaderSize],载荷位于其后。
// 构造时预留头部空间并向前写头,因此 Payload() 返回的切片与 buf 共享底层数组,
// 封装过程中不产生额外拷贝。
//
// ZCPacket 不是并发安全的,不应跨 goroutine 共享同一实例。
type ZCPacket struct {
	buf []byte
	// bufPtr 仅在包由 NewFromPool 创建时非 nil,用于 Release 归还缓冲。
	bufPtr *[]byte
}

// pool 复用 backing array,降低高频发包时的 GC 压力。
var pool = sync.Pool{
	New: func() any {
		b := make([]byte, HeaderSize+DefaultPayloadCap)
		return &b
	},
}

// DefaultPayloadCap 是池化缓冲的默认载荷容量。
const DefaultPayloadCap = 1400

// New 以一个已有载荷构造 ZCPacket,载荷会被复制进新分配的缓冲。
//
// 需要零拷贝时应改用 NewWithHeadroom,由调用方持有底层数组并预留头部空间。
// payload 为 nil 或空切片时构造一个空体包(如 TypeKeepalive)。
func New(payload []byte) *ZCPacket {
	buf := make([]byte, HeaderSize+len(payload))
	copy(buf[HeaderSize:], payload)

	p := &ZCPacket{buf: buf}
	p.setMagic()
	p.setVersion()
	return p
}

// NewWithHeadroom 在调用方提供的底层数组上零拷贝构造 ZCPacket。
//
// buf 的前 HeaderSize 字节用于写入头部,其余部分为载荷区;payloadLen 指明
// 载荷实际长度(须满足 HeaderSize+payloadLen <= len(buf))。调用方须保证 buf
// 在包存活期间不被它用,否则会造成数据竞争。
//
// 这是接收路径与高频发送路径的推荐入口。
func NewWithHeadroom(buf []byte, payloadLen int) (*ZCPacket, error) {
	if payloadLen < 0 {
		return nil, fmt.Errorf("packet: headroom: negative payload length %d", payloadLen)
	}
	if payloadLen > MaxPayloadSize {
		return nil, fmt.Errorf("packet: headroom: %d bytes: %w", payloadLen, ErrPayloadTooLarge)
	}
	if len(buf) < HeaderSize+payloadLen {
		return nil, fmt.Errorf("packet: headroom: need %d bytes, got %d: %w",
			HeaderSize+payloadLen, len(buf), ErrShortBuffer)
	}

	view := buf[:HeaderSize+payloadLen]
	p := &ZCPacket{buf: view}
	// 头部区域必须清零,避免复用脏数据。
	clear(view[:HeaderSize])
	p.setMagic()
	p.setVersion()
	return p, nil
}

// NewWithCap 构造一个头部已就位、载荷容量为 n 的 ZCPacket,返回该包与
// 可写的载荷切片。调用方写入载荷后无需再设置长度。
//
// 这是发送路径的推荐入口:先拿到载荷切片写数据,再交给 Marshal。
func NewWithCap(n int) (*ZCPacket, []byte) {
	if n < 0 {
		n = 0
	}
	if n > MaxPayloadSize {
		n = MaxPayloadSize
	}
	buf := make([]byte, HeaderSize+n)
	p := &ZCPacket{buf: buf}
	p.setMagic()
	p.setVersion()
	return p, buf[HeaderSize:]
}

// NewFromPool 从对象池取出缓冲构造 ZCPacket。返回的包被 Release 回收前
// 不得继续持有其 Payload 切片。
func NewFromPool(n int) (*ZCPacket, []byte) {
	bp := pool.Get().(*[]byte)
	b := *bp
	if cap(b) < HeaderSize+n {
		b = make([]byte, HeaderSize+n)
	}
	buf := b[:HeaderSize+n]
	clear(buf[:HeaderSize])
	p := &ZCPacket{buf: buf}
	p.bufPtr = bp
	p.setMagic()
	p.setVersion()
	return p, buf[HeaderSize:]
}

// Release 将内部缓冲归还对象池。仅对 NewFromPool 创建的包有效。
// 归还后该 ZCPacket 不可再使用。
func (p *ZCPacket) Release() {
	if p.bufPtr == nil {
		return
	}
	pool.Put(p.bufPtr)
	p.bufPtr = nil
	p.buf = nil
}

// Bytes 返回底层缓冲的完整视图(头 + 载荷)。
//
// 返回的切片与 ZCPacket 共享底层数组,调用方不得在包生命周期之外持有它。
func (p *ZCPacket) Bytes() []byte { return p.buf }

// Header 返回头部切片,长度恒为 HeaderSize,与底层缓冲共享数组。
func (p *ZCPacket) Header() []byte { return p.buf[:HeaderSize] }

// Payload 返回载荷切片,与底层缓冲共享数组。
//
// 无载荷时返回长度为 0 的切片(非 nil)。调用方不得长期持有该切片:
// Release 之后底层数组会归还对象池。
func (p *ZCPacket) Payload() []byte {
	if len(p.buf) <= HeaderSize {
		return p.buf[HeaderSize:HeaderSize]
	}
	return p.buf[HeaderSize:]
}

// Len 返回整包长度(头 + 载荷)。
func (p *ZCPacket) Len() int { return len(p.buf) }

// Marshal 返回可写入传输层的字节。为兼容 net.Conn 语义返回 (n, err)。
func (p *ZCPacket) Marshal() ([]byte, error) {
	if len(p.buf) < HeaderSize {
		return nil, fmt.Errorf("packet: marshal: %w", ErrShortBuffer)
	}
	return p.buf, nil
}

// Unmarshal 从字节流解析出 ZCPacket。
//
// 解析不复制数据:返回的 ZCPacket 直接引用 data,调用方不得在包存活期间
// 修改 data。这与零拷贝目标一致,但要求接收缓冲的生命周期受调用方管理。
func Unmarshal(data []byte) (*ZCPacket, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("packet: unmarshal: got %d bytes: %w", len(data), ErrShortBuffer)
	}
	p := &ZCPacket{buf: data}
	if p.Magic() != Magic {
		return nil, fmt.Errorf("packet: unmarshal: got 0x%02X: %w", p.Magic(), ErrBadMagic)
	}
	if p.Version() != ProtoVersion {
		return nil, fmt.Errorf("packet: unmarshal: got %d: %w", p.Version(), ErrBadVersion)
	}
	if len(data)-HeaderSize > MaxPayloadSize {
		return nil, fmt.Errorf("packet: unmarshal: %d bytes: %w", len(data)-HeaderSize, ErrPayloadTooLarge)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// 头部字段访问器
// ---------------------------------------------------------------------------

// Magic 返回包头魔数。
func (p *ZCPacket) Magic() byte { return p.buf[offMagic] }

// Version 返回协议版本。
func (p *ZCPacket) Version() byte { return p.buf[offVer] }

// Type 返回包类型。
func (p *ZCPacket) Type() byte { return p.buf[offType] }

// Flags 返回位标志。
func (p *ZCPacket) Flags() byte { return p.buf[offFlags] }

// SessionID 返回会话标识。
func (p *ZCPacket) SessionID() uint32 { return binary.BigEndian.Uint32(p.buf[offSessionID:]) }

// Sequence 返回单调递增序号。
func (p *ZCPacket) Sequence() uint32 { return binary.BigEndian.Uint32(p.buf[offSequence:]) }

// Ack 返回累计确认序号。
func (p *ZCPacket) Ack() uint32 { return binary.BigEndian.Uint32(p.buf[offAck:]) }

// SrcNodeID 返回源节点标识(16 字节)。返回切片与底层缓冲共享数组。
func (p *ZCPacket) SrcNodeID() []byte { return p.buf[offSrcNode : offSrcNode+NodeIDSize] }

// DstNodeID 返回目的节点标识(16 字节)。返回切片与底层缓冲共享数组。
func (p *ZCPacket) DstNodeID() []byte { return p.buf[offDstNode : offDstNode+NodeIDSize] }

// SetMagic 设置包头魔数。
func (p *ZCPacket) SetMagic(v byte) { p.buf[offMagic] = v }

// SetVersion 设置协议版本。
func (p *ZCPacket) SetVersion(v byte) { p.buf[offVer] = v }

// SetType 设置包类型。
func (p *ZCPacket) SetType(v byte) { p.buf[offType] = v }

// SetFlags 覆盖位标志。
func (p *ZCPacket) SetFlags(v byte) { p.buf[offFlags] = v }

// SetSessionID 设置会话标识。
func (p *ZCPacket) SetSessionID(v uint32) { binary.BigEndian.PutUint32(p.buf[offSessionID:], v) }

// SetSequence 设置序号。
func (p *ZCPacket) SetSequence(v uint32) { binary.BigEndian.PutUint32(p.buf[offSequence:], v) }

// SetAck 设置累计确认序号。
func (p *ZCPacket) SetAck(v uint32) { binary.BigEndian.PutUint32(p.buf[offAck:], v) }

// SetSrcNodeID 设置源节点标识。v 长度必须为 NodeIDSize。
func (p *ZCPacket) SetSrcNodeID(v []byte) error {
	if len(v) != NodeIDSize {
		return fmt.Errorf("packet: src node id: want %d bytes, got %d", NodeIDSize, len(v))
	}
	copy(p.buf[offSrcNode:offSrcNode+NodeIDSize], v)
	return nil
}

// SetDstNodeID 设置目的节点标识。v 长度必须为 NodeIDSize。
func (p *ZCPacket) SetDstNodeID(v []byte) error {
	if len(v) != NodeIDSize {
		return fmt.Errorf("packet: dst node id: want %d bytes, got %d", NodeIDSize, len(v))
	}
	copy(p.buf[offDstNode:offDstNode+NodeIDSize], v)
	return nil
}

// SetFlag 置位指定标志。
func (p *ZCPacket) SetFlag(f byte) { p.buf[offFlags] |= f }

// ClearFlag 清除指定标志。
func (p *ZCPacket) ClearFlag(f byte) { p.buf[offFlags] &^= f }

// HasFlag 判断指定标志是否置位。
func (p *ZCPacket) HasFlag(f byte) bool { return p.buf[offFlags]&f != 0 }

// String 返回便于调试的摘要,不包含载荷内容。
func (p *ZCPacket) String() string {
	return fmt.Sprintf("ZCPacket{type=%d flags=0x%02X session=%d seq=%d ack=%d payload=%d}",
		p.Type(), p.Flags(), p.SessionID(), p.Sequence(), p.Ack(), len(p.Payload()))
}

func (p *ZCPacket) setMagic()   { p.buf[offMagic] = Magic }
func (p *ZCPacket) setVersion() { p.buf[offVer] = ProtoVersion }

// TypeName 返回包类型的中文名称,用于日志与 CLI 输出。
func TypeName(t byte) string {
	switch t {
	case TypeHandshake:
		return "handshake"
	case TypeHandshakeResp:
		return "handshake-resp"
	case TypeData:
		return "data"
	case TypeKeepalive:
		return "keepalive"
	case TypeLSA:
		return "lsa"
	case TypePunch:
		return "punch"
	case TypeRelay:
		return "relay"
	case TypeProbe:
		return "probe"
	case TypeProbeAck:
		return "probe-ack"
	case TypeClose:
		return "close"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}
