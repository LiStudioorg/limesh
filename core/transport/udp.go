package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// UDPTransport 是基于 UDP 的传输实现。
//
// UDP 是默认首选传输:延迟最低、开销最小,且打洞能力最强(NAT 映射直接对应)。
// 它不作任何可靠性保证,可靠性由上层按 Caps.Reliable=false 处理。
//
// 本实现不做加密、不做握手:这些属于后续阶段的数据面与安全层职责。
type UDPTransport struct {
	opts Options
}

// init 注册 UDP 传输。
func init() {
	Register(TypeUDP, func(opts Options) (Transport, error) {
		return NewUDP(opts), nil
	})
}

// NewUDP 构造一个 UDP 传输。
func NewUDP(opts Options) *UDPTransport {
	if opts.ReadBuffer == 0 {
		opts.ReadBuffer = DefaultReadBuffer
	}
	if opts.WriteBuffer == 0 {
		opts.WriteBuffer = DefaultWriteBuffer
	}
	return &UDPTransport{opts: opts}
}

// Type 返回传输类型标识。
func (t *UDPTransport) Type() TransportType { return TypeUDP }

// Capabilities 声明 UDP 的能力。
//
// UDP 不保证可靠与顺序,也不支持多路复用,因此数据面必须自行处理重传
// (仅控制消息)与接收端重排。
func (t *UDPTransport) Capabilities() Caps {
	return Caps{
		Reliable:    false,
		Ordered:     false,
		Multiplexed: false,
		MTU:         1380,
	}
}

// Listen 在 addr 上监听入站连接。
//
// addr 格式为 host:port;host 为空时表示所有接口。双栈监听通过按地址
// 推导网络类型实现:显式指定 IPv6 或 IPv4 地址时只监听对应协议族。
func (t *UDPTransport) Listen(addr string) (Listener, error) {
	network := NetworkFor("udp", t.opts.Network, addr)

	uaddr, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		return nil, fmt.Errorf("transport: udp listen %q: resolve: %w", addr, err)
	}

	conn, err := net.ListenUDP(network, uaddr)
	if err != nil {
		return nil, fmt.Errorf("transport: udp listen %q: %w", addr, err)
	}

	// 放大 socket 缓冲:默认值在突发流量下会造成丢包。
	// 失败不致命,交由调用方决定是否记录。
	_ = setBufferSizes(conn, t.opts.ReadBuffer, t.opts.WriteBuffer)

	return &udpListener{conn: conn, transport: t}, nil
}

// Dial 主动连接到 addr。
//
// UDP 是无连接协议,Dial 只解析地址并绑定本地 socket,不产生网络往返。
// 因此该调用不会失败于对端不可达——可达性由后续的收发探测判定。
func (t *UDPTransport) Dial(ctx context.Context, addr string) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("transport: udp dial %q: %w", addr, err)
	}

	network := NetworkFor("udp", t.opts.Network, addr)

	raddr, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		return nil, fmt.Errorf("transport: udp dial %q: resolve: %w", addr, err)
	}

	conn, err := net.DialUDP(network, nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("transport: udp dial %q: %w", addr, err)
	}

	_ = setBufferSizes(conn, t.opts.ReadBuffer, t.opts.WriteBuffer)

	return newUDPConn(conn), nil
}

// udpConn 实现 Conn 接口。
type udpConn struct {
	conn *net.UDPConn

	bytesSent atomic.Uint64
	bytesRecv atomic.Uint64

	closeMu sync.Mutex
	closed  atomic.Bool
}

// newUDPConn 包装一个已连接的 UDP socket。
func newUDPConn(c *net.UDPConn) *udpConn {
	return &udpConn{conn: c}
}

// Read 读取一个数据报。p 必须足够大以容纳整个数据报,否则多余部分被丢弃
// ——这是 UDP 的固有语义,不是实现缺陷。
func (c *udpConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, fmt.Errorf("transport: udp read: %w", ErrClosed)
	}
	n, err := c.conn.Read(p)
	if n > 0 {
		c.bytesRecv.Add(uint64(n))
	}
	if err != nil {
		return n, fmt.Errorf("transport: udp read: %w", err)
	}
	return n, nil
}

// Write 写入一个数据报。实现不会在返回后引用 p。
func (c *udpConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, fmt.Errorf("transport: udp write: %w", ErrClosed)
	}
	n, err := c.conn.Write(p)
	if n > 0 {
		c.bytesSent.Add(uint64(n))
	}
	if err != nil {
		return n, fmt.Errorf("transport: udp write: %w", err)
	}
	return n, nil
}

// Close 关闭连接。重复调用是安全的。
func (c *udpConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	if c.closed.Swap(true) {
		return nil
	}
	if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("transport: udp close: %w", err)
	}
	return nil
}

// LocalAddr 返回本地地址。
func (c *udpConn) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// RemoteAddr 返回对端地址。
func (c *udpConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetReadDeadline 设置读操作的截止时间。传零值表示不设期限。
func (c *udpConn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return fmt.Errorf("transport: udp set deadline: %w", ErrClosed)
	}
	if err := c.conn.SetReadDeadline(t); err != nil {
		return fmt.Errorf("transport: udp set deadline: %w", err)
	}
	return nil
}

// Stats 返回连接统计。
//
// UDP 无连接状态,因此 RTT 与 Loss 无法在传输层测得,保持零值;
// 这两项由上层的 Probe 采样填充(见 AGENTS.md 8.6.5)。
func (c *udpConn) Stats() ConnStats {
	return ConnStats{
		BytesSent: c.bytesSent.Load(),
		BytesRecv: c.bytesRecv.Load(),
	}
}

// udpListener 实现 Listener 接口。
type udpListener struct {
	conn      *net.UDPConn
	transport *UDPTransport

	// pending 缓存 Accept 读取到的首个数据报,交由返回的 Conn 首次 Read 消费。
	// 监听 socket 上的其余数据报同样按来源地址分派给对应 Conn。
	pendingMu sync.Mutex
	pending   map[string][][]byte

	closeMu sync.Mutex
	closed  atomic.Bool
}

// Accept 等待一个数据报并把其来源地址固定为一个 Conn。
//
// UDP 没有连接建立过程,因此 Accept 的语义是"识别首个对端":读到首个数据报后
// 把来源地址记为对端,该连接与其后的收发都复用监听 socket,只是过滤地址。
//
// 关键实现约束:不能用新 socket 代替监听 socket。UDP 数据报只会投递到创建时
// 绑定的那个 socket,新 socket 收不到已在监听地址上排队的数据。因此这里按
// 来源地址分派,复用同一 socket。
func (l *udpListener) Accept(ctx context.Context) (Conn, error) {
	if l.closed.Load() {
		return nil, fmt.Errorf("transport: udp accept: %w", ErrClosed)
	}

	buf := make([]byte, 65535)
	var raddr *net.UDPAddr

	// 轮询读取,直到拿到数据报或 ctx 结束——这样 ctx 取消能被及时响应。
	for {
		if err := l.applyDeadline(ctx); err != nil {
			return nil, err
		}
		n, addr, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if l.closed.Load() {
				return nil, fmt.Errorf("transport: udp accept: %w", ErrClosed)
			}
			// 读超时是正常的轮询节拍,继续等待。
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				if cerr := ctx.Err(); cerr != nil {
					return nil, fmt.Errorf("transport: udp accept: %w", cerr)
				}
				continue
			}
			return nil, fmt.Errorf("transport: udp accept: %w", err)
		}

		raddr = addr
		// 缓存首个数据报,使返回的 Conn 被 Read 时不会丢失该载荷。
		payload := make([]byte, n)
		copy(payload, buf[:n])

		l.pendingMu.Lock()
		if l.pending == nil {
			l.pending = map[string][][]byte{}
		}
		l.pending[addr.String()] = append(l.pending[addr.String()], payload)
		l.pendingMu.Unlock()
		break
	}

	return &udpListenerConn{
		listener: l,
		remote:   raddr,
	}, nil
}

// takePending 取出并清空指定来源已缓存的数据报。
func (l *udpListener) takePending(key string) [][]byte {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()

	got := l.pending[key]
	delete(l.pending, key)
	return got
}

// udpListenerConn 是被 Accept 识别的对端连接,复用监听 socket。
//
// 与 udpConn 的区别在于它带地址过滤:只接收来自 remote 的数据报,
// 其它来源的数据报被丢弃(Phase 0 尚不支持单 socket 多对端)。
type udpListenerConn struct {
	listener *udpListener
	remote   *net.UDPAddr

	bytesSent atomic.Uint64
	bytesRecv atomic.Uint64
	closed    atomic.Bool
}

// Read 读取一个来自已识别对端的数据报。
func (c *udpListenerConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, fmt.Errorf("transport: udp read: %w", ErrClosed)
	}

	// 优先交付 Accept 阶段缓存的首个数据报。
	if pending := c.listener.takePending(c.remote.String()); len(pending) > 0 {
		n := copy(p, pending[0])
		// 未装下的部分重新排队,避免静默丢弃。
		if rest := pending[1:]; len(rest) > 0 {
			c.listener.pendingMu.Lock()
			if c.listener.pending == nil {
				c.listener.pending = map[string][][]byte{}
			}
			c.listener.pending[c.remote.String()] = append(rest, c.listener.pending[c.remote.String()]...)
			c.listener.pendingMu.Unlock()
		}
		c.bytesRecv.Add(uint64(n))
		return n, nil
	}

	buf := make([]byte, 65535)
	for {
		if c.closed.Load() {
			return 0, fmt.Errorf("transport: udp read: %w", ErrClosed)
		}
		n, addr, err := c.listener.conn.ReadFromUDP(buf)
		if err != nil {
			if c.listener.closed.Load() {
				return 0, fmt.Errorf("transport: udp read: %w", ErrClosed)
			}
			return 0, fmt.Errorf("transport: udp read: %w", err)
		}
		// 过滤非本连接来源的数据报。
		if addr.String() != c.remote.String() {
			continue
		}
		got := copy(p, buf[:n])
		c.bytesRecv.Add(uint64(got))
		return got, nil
	}
}

// Write 向已识别对端发送一个数据报。
func (c *udpListenerConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, fmt.Errorf("transport: udp write: %w", ErrClosed)
	}
	n, err := c.listener.conn.WriteToUDP(p, c.remote)
	if n > 0 {
		c.bytesSent.Add(uint64(n))
	}
	if err != nil {
		return n, fmt.Errorf("transport: udp write: %w", err)
	}
	return n, nil
}

// Close 关闭该对端连接。监听 socket 本身保持打开。
func (c *udpListenerConn) Close() error {
	c.closed.Store(true)
	return nil
}

// LocalAddr 返回监听 socket 的本地地址。
func (c *udpListenerConn) LocalAddr() net.Addr { return c.listener.conn.LocalAddr() }

// RemoteAddr 返回已识别的对端地址。
func (c *udpListenerConn) RemoteAddr() net.Addr { return c.remote }

// SetReadDeadline 设置读操作的截止时间。
func (c *udpListenerConn) SetReadDeadline(t time.Time) error {
	if err := c.listener.conn.SetReadDeadline(t); err != nil {
		return fmt.Errorf("transport: udp set deadline: %w", err)
	}
	return nil
}

// Stats 返回连接统计。
func (c *udpListenerConn) Stats() ConnStats {
	return ConnStats{
		BytesSent: c.bytesSent.Load(),
		BytesRecv: c.bytesRecv.Load(),
	}
}

// applyDeadline 依据 ctx 设置读超时,使 Accept 可被取消。
func (l *udpListener) applyDeadline(ctx context.Context) error {
	if dl, ok := ctx.Deadline(); ok {
		if err := l.conn.SetReadDeadline(dl); err != nil {
			return fmt.Errorf("transport: udp accept: set deadline: %w", err)
		}
		return nil
	}
	// 无 deadline 时用轮询间隔,保证 ctx 取消能被及时响应。
	if err := l.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		return fmt.Errorf("transport: udp accept: set deadline: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DatagramConn 实现
//
// udpListener 同时满足 DatagramConn,使数据面可以在一个端口上收发任意来源的
// 数据报(以 SrcNodeID 区分对端),而不必为每个对端 Accept 一条连接。
// 这两条路径互不干扰:Accept/Conn 仍为需要连接语义的调用方保留。
// ---------------------------------------------------------------------------

// ReadFrom 读取一个数据报并返回来源地址。不做任何来源过滤。
func (l *udpListener) ReadFrom(p []byte) (int, net.Addr, error) {
	if l.closed.Load() {
		return 0, nil, fmt.Errorf("transport: udp readfrom: %w", ErrClosed)
	}
	n, addr, err := l.conn.ReadFromUDP(p)
	if err != nil {
		if l.closed.Load() {
			return 0, nil, fmt.Errorf("transport: udp readfrom: %w", ErrClosed)
		}
		return 0, nil, fmt.Errorf("transport: udp readfrom: %w", err)
	}
	return n, addr, nil
}

// WriteTo 向 addr 发送一个数据报。
func (l *udpListener) WriteTo(p []byte, addr net.Addr) (int, error) {
	if l.closed.Load() {
		return 0, fmt.Errorf("transport: udp writeto: %w", ErrClosed)
	}
	uaddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("transport: udp writeto: unsupported addr type %T", addr)
	}
	n, err := l.conn.WriteToUDP(p, uaddr)
	if err != nil {
		return n, fmt.Errorf("transport: udp writeto: %w", err)
	}
	return n, nil
}

// SetReadDeadline 设置读截止时间,复用 Conn 的语义。
func (l *udpListener) SetReadDeadline(t time.Time) error {
	if err := l.conn.SetReadDeadline(t); err != nil {
		return fmt.Errorf("transport: udp listener set deadline: %w", err)
	}
	return nil
}

// LocalAddr 是 DatagramConn 要求的访问器,与 Addr 返回同一地址。
func (l *udpListener) LocalAddr() net.Addr { return l.conn.LocalAddr() }

// Addr 返回监听地址。
func (l *udpListener) Addr() net.Addr { return l.conn.LocalAddr() }

// Close 停止监听。重复调用是安全的。
func (l *udpListener) Close() error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()

	if l.closed.Swap(true) {
		return nil
	}
	if err := l.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("transport: udp listener close: %w", err)
	}
	return nil
}
