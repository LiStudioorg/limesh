// Package nat 实现 Limesh 的 NAT 穿透基础层(L2):STUN 客户端、NAT 类型探测
// 与候选地址收集。
//
// Phase 2a 的边界:只做"观测"——查明本端在 NAT 后的映射地址与 NAT 类型、
// 收集本机可用候选地址。真正的打洞(2b)、中继(2c)、UPnP(2d)在此之上构建。
//
// 设计说明见 AGENTS.md 8.6.3;配置见 docs/CONFIG.md 6.6。
package nat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v2"
)

// STUN 相关错误。
var (
	// ErrNoServer 表示未配置任何 STUN 服务器。
	ErrNoServer = errors.New("nat: no STUN server configured")
	// ErrNoResponse 表示所有 STUN 服务器均无响应。
	ErrNoResponse = errors.New("nat: no STUN response")
)

// DefaultTimeout 是单次 STUN 查询的默认超时。
const DefaultTimeout = 3 * time.Second

// MappedAddr 描述 STUN 服务器观察到的本端公网映射地址。
type MappedAddr struct {
	// IP 是映射后的公网 IP。
	IP netip.Addr
	// Port 是映射后的公网端口。
	Port uint16
	// Server 是返回该映射的 STUN 服务器地址。
	Server string
	// RTT 是该次查询的往返延迟。
	RTT time.Duration
}

// AddrPort 返回映射地址的 netip 形式。
func (m *MappedAddr) AddrPort() netip.AddrPort {
	if m == nil {
		return netip.AddrPort{}
	}
	return netip.AddrPortFrom(m.IP, m.Port)
}

// String 返回 host:port 形式。
func (m *MappedAddr) String() string {
	if m == nil {
		return ""
	}
	return m.AddrPort().String()
}

// STUNClient 是一个 STUN 客户端。
//
// **关键设计:所有查询复用同一个 UDP socket。** NAT 类型探测的核心正是
// "同一个本地端口经不同 STUN 服务器观察到的映射是否一致"——若每次查询新建
// socket,本地端口变了,映射自然不同,无法判断 NAT 类型。
//
// 由于 socket 是共享的,响应必须由一个**单一的读循环**接收并按 (来源地址,
// 事务 ID) 分派给等待的查询。若让每个查询各自读同一 socket,并发查询会互相
// 抢走对方的响应,导致随机超时。
type STUNClient struct {
	servers []string
	timeout time.Duration

	mu   sync.Mutex
	conn *net.UDPConn
	// pending 按事务 ID 索引等待中的查询。
	pending map[[stun.TransactionIDSize]byte]chan *stun.Message
	// readerStarted 标记读循环是否已启动。
	readerStarted bool
	// closed 标记客户端已关闭。
	closed bool
}

// NewSTUNClient 创建一个 STUN 客户端。
//
// servers 接受 host:port 或 stun:host:port。空列表返回 ErrNoServer。
func NewSTUNClient(servers []string) (*STUNClient, error) {
	if len(servers) == 0 {
		return nil, ErrNoServer
	}
	return &STUNClient{
		servers: append([]string(nil), servers...),
		timeout: DefaultTimeout,
		pending: make(map[[stun.TransactionIDSize]byte]chan *stun.Message),
	}, nil
}

// SetTimeout 覆盖默认查询超时。
func (c *STUNClient) SetTimeout(d time.Duration) {
	if d > 0 {
		c.timeout = d
	}
}

// Servers 返回已配置的服务器列表。
func (c *STUNClient) Servers() []string {
	return append([]string(nil), c.servers...)
}

// ensureConn 惰性创建复用的 UDP socket 并启动单一读循环。
func (c *STUNClient) ensureConn() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("nat: stun client closed")
	}
	if c.conn != nil {
		return nil
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return fmt.Errorf("nat: open udp socket: %w", err)
	}
	c.conn = conn
	c.readerStarted = true
	go c.readLoop(conn)
	return nil
}

// readLoop 是唯一的 socket 读循环:按事务 ID 把响应分派给等待者。
func (c *STUNClient) readLoop(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			// 关闭或永久错误:退出。
			return
		}
		resp := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := resp.Decode(); err != nil {
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[resp.TransactionID]
		c.mu.Unlock()
		if !ok {
			continue
		}
		// 非阻塞投递:等待者若已超时离开,直接丢弃。
		select {
		case ch <- resp:
		default:
		}
	}
}

// register 为指定事务 ID 注册一个等待通道。
func (c *STUNClient) register(id [stun.TransactionIDSize]byte) chan *stun.Message {
	ch := make(chan *stun.Message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

// unregister 移除等待通道。
func (c *STUNClient) unregister(id [stun.TransactionIDSize]byte) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// LocalPort 返回复用 socket 的本地端口;未创建时返回 0。
//
// 该端口用于 NAT 类型探测的"本地映射"比较。读取受锁保护,可与 Close 并发。
func (c *STUNClient) LocalPort() uint16 {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0
	}
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return uint16(ua.Port)
	}
	return 0
}

// LocalIP 返回复用 socket 出站使用的本地 IP;未创建时返回空串。
//
// 通过与一个 STUN 服务器建立"已连接"的 UDP 视图来取得内核选定的源地址,
// 不发送任何数据。用于判断映射地址是否等于本机地址(即无 NAT)。
func (c *STUNClient) LocalIP() string {
	c.mu.Lock()
	conn := c.conn
	servers := c.servers
	c.mu.Unlock()
	if conn == nil || len(servers) == 0 {
		return ""
	}
	raddr, err := resolveServer(servers[0])
	if err != nil {
		return ""
	}
	// 用临时连接探测源地址(Dial UDP 不发包)。
	probe, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return ""
	}
	defer probe.Close()
	if la, ok := probe.LocalAddr().(*net.UDPAddr); ok && la.IP != nil {
		ip := la.IP
		if ip.IsUnspecified() || ip.IsLoopback() {
			return ""
		}
		return ip.String()
	}
	return ""
}

// Close 关闭复用 socket。重复调用是安全的。
func (c *STUNClient) Close() error {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.closed = true
	c.mu.Unlock()

	if conn == nil {
		return nil
	}
	return conn.Close()
}

// Query 查询单个 STUN 服务器,返回本端公网映射。
func (c *STUNClient) Query(ctx context.Context, server string) (*MappedAddr, error) {
	if err := c.ensureConn(); err != nil {
		return nil, err
	}

	raddr, err := resolveServer(server)
	if err != nil {
		return nil, err
	}

	// 构造 STUN Binding Request。
	msg, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return nil, fmt.Errorf("nat: build binding request: %w", err)
	}

	// 注册等待通道(在读循环启动后)。
	ch := c.register(msg.TransactionID)
	defer c.unregister(msg.TransactionID)

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("nat: query %s: client closed", server)
	}

	start := time.Now()
	if _, err := conn.WriteToUDP(msg.Raw, raddr); err != nil {
		return nil, fmt.Errorf("nat: send to %s: %w", server, err)
	}

	// 每次查询独立超时。
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("nat: query %s: %w", server, ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("nat: query %s: %w", server, ErrNoResponse)
	case resp := <-ch:
		if resp.Type.Class != stun.ClassSuccessResponse {
			return nil, fmt.Errorf("nat: query %s: unexpected class %s", server, resp.Type.Class)
		}

		var addr stun.XORMappedAddress
		if err := addr.GetFrom(resp); err != nil {
			return nil, fmt.Errorf("nat: query %s: xor-mapped-address: %w", server, err)
		}

		ip, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			return nil, fmt.Errorf("nat: query %s: invalid mapped IP %v", server, addr.IP)
		}
		// IPv4-in-IPv6 归一为纯 IPv4。
		ip = ip.Unmap()

		return &MappedAddr{
			IP:     ip,
			Port:   uint16(addr.Port),
			Server: server,
			RTT:    time.Since(start),
		}, nil
	}
}

// QueryAny 并发查询所有服务器,返回**第一个成功**的结果。
//
// 所有查询共享同一 socket,因此映射一致;先返回者胜出,RTT 最短者通常最先
// 返回。至少两个服务器都失败才返回错误。
func (c *STUNClient) QueryAny(ctx context.Context) (*MappedAddr, error) {
	if len(c.servers) == 0 {
		return nil, ErrNoServer
	}
	if err := c.ensureConn(); err != nil {
		return nil, err
	}

	type result struct {
		addr *MappedAddr
		err  error
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan result, len(c.servers))
	for _, s := range c.servers {
		go func(server string) {
			m, err := c.Query(ctx, server)
			ch <- result{addr: m, err: err}
		}(s)
	}

	var lastErr error
	for i := 0; i < len(c.servers); i++ {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("nat: query any: %w", ctx.Err())
		case r := <-ch:
			if r.err == nil {
				return r.addr, nil
			}
			lastErr = r.err
		}
	}
	if lastErr == nil {
		lastErr = ErrNoResponse
	}
	return nil, fmt.Errorf("nat: query any: %w", lastErr)
}

// resolveServer 把 "stun:host:port" 或 "host:port" 解析为 UDP 地址。
func resolveServer(server string) (*net.UDPAddr, error) {
	host := strings.TrimPrefix(server, "stun:")
	raddr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		return nil, fmt.Errorf("nat: resolve %q: %w", server, err)
	}
	return raddr, nil
}
