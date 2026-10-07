// Package transport 实现 Limesh 的传输层(L3)。
//
// 传输层的职责只有一个:把 ZCPacket 的字节从一个节点搬到另一个节点。
// 它不解析 IP 包、不做路由决策、不参与 ACL 判定。上层依据 Caps 声明决定
// 是否叠加重传、重排或分片,因此新增一种传输不必改动路由与加密。
//
// 设计说明见 AGENTS.md 第 9 章,协议详解见 docs/TRANSPORT.md。
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// 传输层错误。
var (
	// ErrClosed 表示传输或连接已关闭。
	ErrClosed = errors.New("transport: closed")
	// ErrUnsupported 表示请求的传输类型未注册或未实现。
	ErrUnsupported = errors.New("transport: unsupported")
)

// TransportType 是传输协议的类型标识。
//
// Phase 0 只实现 UDP 与 TCP 的骨架;其余 16 种在后续阶段加入,
// 取值与 docs/TRANSPORT.md 第 3 章的注册名一一对应。
type TransportType string

// 传输类型常量。注册名与配置中的 transports.protocols 项一致。
const (
	// TypeUDP 是 UDP 传输,延迟最低、无重传。
	TypeUDP TransportType = "udp"
	// TypeTCP 是 TCP 传输,穿透友好、自带可靠保序。
	TypeTCP TransportType = "tcp"
)

// Caps 描述一种传输的能力,数据面据此决定封装策略。
type Caps struct {
	// Reliable 表示该传输自带重传与拥塞控制。
	// 为 true 时数据面不再叠加确认机制。
	Reliable bool
	// Ordered 表示该传输保证字节顺序。
	// 为 false 时数据面必须启用序号重排。
	Ordered bool
	// Multiplexed 表示该传输原生支持多路复用。
	// 为 true 时多个 Peer 可共用一个底层连接。
	Multiplexed bool
	// MTU 是该传输在典型路径上的建议最大载荷。
	MTU int
}

// Conn 是一条已建立的双向传输连接。
type Conn interface {
	// Read 读取一个载荷。返回的切片在下次 Read 前有效。
	Read(p []byte) (n int, err error)
	// Write 写入一个载荷。实现不得在返回后继续引用 p。
	Write(p []byte) (n int, err error)
	// Close 关闭连接。
	Close() error
	// LocalAddr 返回本地地址,用于候选地址收集。
	LocalAddr() net.Addr
	// RemoteAddr 返回对端地址,用于候选地址收集与路径展示。
	RemoteAddr() net.Addr
	// SetReadDeadline 设置读操作的截止时间。
	//
	// 传输层必须支持它:UDP 等无连接传输的 Read 若无期限会永久阻塞,
	// 上层的取消与超时逻辑将无法实现。传零值表示不设期限。
	SetReadDeadline(t time.Time) error
	// Stats 返回该连接的实时统计。
	Stats() ConnStats
}

// ConnStats 是连接级统计,用于观测与路由打分。
type ConnStats struct {
	// RTT 是平滑往返延迟。
	RTT time.Duration
	// Loss 是丢包率,取值 [0,1]。
	Loss float64
	// BytesSent 是已发送字节数。
	BytesSent uint64
	// BytesRecv 是已接收字节数。
	BytesRecv uint64
	// Retransmit 是传输层自身的重传次数。
	Retransmit uint64
}

// Listener 接受入站连接。
type Listener interface {
	// Accept 阻塞等待一条入站连接。
	Accept(ctx context.Context) (Conn, error)
	// Addr 返回监听地址。
	Addr() net.Addr
	// Close 停止监听。
	Close() error
}

// Transport 是所有传输协议必须实现的接口。
type Transport interface {
	// Dial 主动连接到指定对端。addr 格式为 host:port。
	Dial(ctx context.Context, addr string) (Conn, error)
	// Listen 监听入站连接。addr 格式为 host:port。
	Listen(addr string) (Listener, error)
	// Type 返回传输类型标识。
	Type() TransportType
	// Capabilities 声明该传输的能力。
	//
	// 必须如实声明:数据面完全依赖这些字段决定是否叠加重传、重排与分片,
	// 虚报会导致静默错误(例如把不可靠传输标记为 Reliable 会使控制消息丢失)。
	Capabilities() Caps
}

// Factory 构造一个传输实例。
type Factory func(opts Options) (Transport, error)

// Options 是传输的构造参数。
type Options struct {
	// ReadBuffer 是 socket 读缓冲字节数;为 0 时使用 DefaultReadBuffer。
	ReadBuffer int
	// WriteBuffer 是 socket 写缓冲字节数;为 0 时使用 DefaultWriteBuffer。
	WriteBuffer int
	// Network 强制指定网络类型("udp" / "udp4" / "udp6");
	// 为空时由实现按地址自动选择(双栈)。
	Network string
}

// 默认 socket 缓冲大小。
//
// 组网场景下常出现突发流量,默认缓冲(通常 208KB)在高带宽时会造成丢包,
// 因此显式放大。
const (
	// DefaultReadBuffer 是默认读缓冲字节数。
	DefaultReadBuffer = 4 << 20 // 4 MiB
	// DefaultWriteBuffer 是默认写缓冲字节数。
	DefaultWriteBuffer = 4 << 20 // 4 MiB
)

// registry 保存已注册的传输工厂。
var (
	registryMu sync.RWMutex
	registry   = map[TransportType]Factory{}
)

// Register 注册一种传输实现。应在包的 init 中调用。
func Register(t TransportType, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[t] = f
}

// New 按类型构造传输实例。
func New(t TransportType, opts Options) (Transport, error) {
	registryMu.RLock()
	f, ok := registry[t]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, t)
	}
	tr, err := f(opts)
	if err != nil {
		return nil, fmt.Errorf("transport: new %s: %w", t, err)
	}
	return tr, nil
}

// Registered 返回已注册的传输类型列表。
func Registered() []TransportType {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]TransportType, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	return out
}

// NetworkFor 按地址推导网络类型,优先跟随地址的 IP 族。
//
// 传入 "udp4"/"udp6" 时直接使用;空串时按 addr 的 IP 族选择,
// 使双栈监听不需要调用方显式判断。
func NetworkFor(base, network, addr string) string {
	if network != "" {
		return network
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return base
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return base
	}
	if ip.To4() != nil {
		return base + "4"
	}
	return base + "6"
}

// setBufferSizes 尽力放大底层 socket 的读写缓冲。
//
// 失败不应中断启动:部分平台或受限环境下会拒绝该设置,此时保持系统默认值
// 并交由调用方记录日志即可。
func setBufferSizes(c *net.UDPConn, read, write int) error {
	if read > 0 {
		if err := c.SetReadBuffer(read); err != nil {
			return fmt.Errorf("set read buffer: %w", err)
		}
	}
	if write > 0 {
		if err := c.SetWriteBuffer(write); err != nil {
			return fmt.Errorf("set write buffer: %w", err)
		}
	}
	return nil
}
