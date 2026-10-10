package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/handshake"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/peer"
	"github.com/LiStudioorg/limesh/core/session"
	"github.com/LiStudioorg/limesh/core/transport"
)

// 数据面错误。
var (
	// ErrNoKeyPair 表示节点没有可用于握手的长期密钥。
	ErrNoKeyPair = errors.New("node: no key pair configured")
	// ErrNoDatagram 表示当前传输不支持多对端数据报收发。
	ErrNoDatagram = errors.New("node: transport does not support datagram mode")
)

// PeerSpec 描述一个已知对端:地址 + 长期公钥。
//
// Noise IK 要求**发起方预先知道响应方公钥**,因此仅有地址不足以发起握手。
// Phase 1a 尚无可信的发现/控制面下发通道,故对端公钥由调用方显式注入。
type PeerSpec struct {
	// Addr 是对端地址(host:port)。
	Addr string
	// PublicKey 是对端长期 Curve25519 公钥。
	PublicKey [crypto.KeySize]byte
}

// inflight 表示一条进行中的握手。
//
// 握手阶段尚无法从 ZCPacket.SrcNodeID 识别对端(会话未建立),因此用对端
// 地址与握手分配的 SenderIndex 双重索引。
type inflight struct {
	hs     *handshake.Handshake
	spec   PeerSpec
	remote *net.UDPAddr
	// startedAt 用于握手超时清理。
	startedAt time.Time
}

// datapath 是节点的加密数据面。
//
// 职责:持有身份与会话、驱动握手、按包类型分派收发。它不关心 Ingress
// (TUN/netstack):那是后续阶段的工作,Phase 1a 的数据结尾在"解出明文"。
type datapath struct {
	cfg *configView

	local    *crypto.KeyPair
	localID  [crypto.NodeIDSize]byte
	sessions *session.Manager
	peers    *peer.Manager
	log      *zap.Logger

	// datagrams 是启动后建立的、支持多对端收发的监听 socket 集合。
	mu        sync.Mutex
	datagrams []transport.DatagramConn
	// byAddr 把对端地址映射到 datagram,便于向该对端发送。
	addrToDg map[string]transport.DatagramConn

	// inflight 是按对端地址索引的进行中握手。
	hsMu     sync.Mutex
	inflight map[string]*inflight
	// byIndex 用握手 SenderIndex 定位响应方握手。
	byIndex map[uint32]*inflight

	// onPlaintext 在成功解出一个数据包明文后被调用。
	//
	// Phase 1a 默认实现为丢弃(或测试注入的回显)。Ingress 接入后,
	// 这里会替换为"把明文 IP 包写入内核或用户态栈"。
	onPlaintext func(srcID [crypto.NodeIDSize]byte, plaintext []byte)

	closed   bool
	closeMu  sync.Mutex
	stopOnce sync.Once
	stopCh   chan struct{}
}

// configView 是数据面对配置的最小只读视图,避免把整个 config.Config 泄漏进
// 数据面内部逻辑。
type configView struct {
	networkName string
	prologue    []byte
	e2ee        bool
}

// newDatapath 构造数据面。
func newDatapath(local *crypto.KeyPair, cv *configView, log *zap.Logger) *datapath {
	if log == nil {
		log = zap.NewNop()
	}
	return &datapath{
		cfg:      cv,
		local:    local,
		localID:  local.NodeID(),
		sessions: session.NewManager(),
		peers:    peer.NewManager(peer.ManagerOptions{}),
		log:      log,
		addrToDg: map[string]transport.DatagramConn{},
		inflight: map[string]*inflight{},
		byIndex:  map[uint32]*inflight{},
		stopCh:   make(chan struct{}),
	}
}

// LocalNodeID 返回本节点身份。
func (d *datapath) LocalNodeID() [crypto.NodeIDSize]byte { return d.localID }

// DirectPeer 返回 Phase 1b 两点直连场景下的唯一活跃对端。
//
// Phase 1b 不做 OSPF 路由:所有来自 TUN 的流量都发给"唯一已建立会话的对端"。
// 因此这里要求恰好存在一个可用会话;0 个时返回错误(无处可发),多于 1 个时
// 返回错误(无法判断走哪条——真正的多节点路由属 Phase 1c)。
//
// 返回值语义:(peerID, ok, err):
//   - ok=true  :找到唯一对端,peerID 有效;
//   - ok=false :当前无可用会话(调用方应丢弃包或等待握手完成),err 为 nil;
//   - err!=nil :存在歧义(多个对端),无法在 Phase 1b 决策。
func (d *datapath) DirectPeer() (peerID [crypto.NodeIDSize]byte, ok bool, err error) {
	ids := d.sessions.IDs()

	var found [crypto.NodeIDSize]byte
	count := 0
	for _, id := range ids {
		s, ok := d.sessions.Get(id)
		if !ok || s.Closed() {
			continue
		}
		if count == 0 {
			found = s.PeerID
		}
		count++
	}

	switch count {
	case 0:
		return [crypto.NodeIDSize]byte{}, false, nil
	case 1:
		return found, true, nil
	default:
		return [crypto.NodeIDSize]byte{}, false,
			fmt.Errorf("node: direct peer ambiguous: %d sessions (routing is Phase 1c)", count)
	}
}

// Sessions 返回会话池,供上层观测。
func (d *datapath) Sessions() *session.Manager { return d.sessions }

// Peers 返回对端注册表,供上层观测。
func (d *datapath) Peers() *peer.Manager { return d.peers }

// setPlaintextHandler 注册明文回调。必须在数据面启动前调用。
func (d *datapath) setPlaintextHandler(fn func([crypto.NodeIDSize]byte, []byte)) {
	d.onPlaintext = fn
}

// addDatagram 注册一个支持数据报收发的监听 socket。
func (d *datapath) addDatagram(dg transport.DatagramConn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.datagrams = append(d.datagrams, dg)
}

// datagramFor 返回用于向 addr 发送的 datagram。
//
// Phase 1a 只有一个数据面 socket,直接返回它;多监听场景下按地址族选择
// 更合适的 socket 留待后续阶段。
func (d *datapath) datagramFor(addr net.Addr) transport.DatagramConn {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.datagrams) == 0 {
		return nil
	}
	return d.datagrams[0]
}

// registerDatagramAddr 记录地址到 socket 的映射(用于日志与多 socket 选择)。
func (d *datapath) registerDatagramAddr(addr string, dg transport.DatagramConn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrToDg[addr] = dg
}

// ---------------------------------------------------------------------------
// 发送路径
// ---------------------------------------------------------------------------

// sendRaw 把一个已编码的 ZCPacket 写到指定地址。
func (d *datapath) sendRaw(pkt *packet.ZCPacket, addr net.Addr) error {
	dg := d.datagramFor(addr)
	if dg == nil {
		return ErrNoDatagram
	}
	wire, err := pkt.Marshal()
	if err != nil {
		return fmt.Errorf("node: marshal: %w", err)
	}
	if _, err := dg.WriteTo(wire, addr); err != nil {
		return fmt.Errorf("node: send to %s: %w", addr, err)
	}
	return nil
}

// sendHandshake 发送一条握手消息到 addr。
func (d *datapath) sendHandshake(pktType byte, payload []byte, addr net.Addr) error {
	pkt := packet.New(payload)
	pkt.SetType(pktType)
	pkt.SetFlags(packet.FlagEncrypted)
	if err := pkt.SetSrcNodeID(d.localID[:]); err != nil {
		return fmt.Errorf("node: set src node id: %w", err)
	}
	return d.sendRaw(pkt, addr)
}

// SendData 使用对端的活跃会话加密并发送一个数据包。
//
// 发送路径一律使用 Session.SealNext:它在同一临界区内完成"取号 + 加密",
// 是多 goroutine 并发发送时唯一不会触发 nonce 重用的用法(见 session 包文档)。
func (d *datapath) SendData(peerID [crypto.NodeIDSize]byte, plaintext []byte) error {
	s, ok := d.sessions.GetByPeer(peerID)
	if !ok {
		return fmt.Errorf("node: send data: no session for peer %s", crypto.NodeIDString(peerID))
	}
	if s.Closed() {
		return fmt.Errorf("node: send data: session closed for peer %s", crypto.NodeIDString(peerID))
	}

	addr, ok := d.peerAddr(peerID)
	if !ok {
		return fmt.Errorf("node: send data: no address for peer %s", crypto.NodeIDString(peerID))
	}

	p := d.peers
	if p == nil {
		return fmt.Errorf("node: send data: peer manager unavailable")
	}

	// 以 ZCPacket 头的寻址字段作为 AAD,防止头部被篡改。
	aad := buildAAD(d.localID, peerID, s.ID)

	seq, ciphertext, err := s.SealNext(plaintext, aad)
	if err != nil {
		return fmt.Errorf("node: seal data: %w", err)
	}

	pkt := packet.New(ciphertext)
	pkt.SetType(packet.TypeData)
	pkt.SetFlags(packet.FlagEncrypted)
	pkt.SetSessionID(s.ID)
	pkt.SetSequence(uint32(seq))
	if err := pkt.SetSrcNodeID(d.localID[:]); err != nil {
		return err
	}
	if err := pkt.SetDstNodeID(peerID[:]); err != nil {
		return err
	}
	return d.sendRaw(pkt, addr)
}

// peerAddr 返回对端地址。
func (d *datapath) peerAddr(peerID [crypto.NodeIDSize]byte) (net.Addr, bool) {
	p, ok := d.peers.Get(peerID)
	if !ok {
		return nil, false
	}
	addr := p.Addr()
	if addr == "" {
		return nil, false
	}
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, false
	}
	return uaddr, true
}

// buildAAD 构造参与 AEAD 认证的附加数据:源/目的 NodeID + 会话 ID。
//
// 把寻址字段纳入 AAD,使中间人无法把包改写成"投递给别人"而不被察觉
// (AGENTS.md 8.3)。
func buildAAD(src, dst [crypto.NodeIDSize]byte, sessionID uint32) []byte {
	aad := make([]byte, 0, crypto.NodeIDSize*2+4)
	aad = append(aad, src[:]...)
	aad = append(aad, dst[:]...)
	var b [4]byte
	b[0] = byte(sessionID >> 24)
	b[1] = byte(sessionID >> 16)
	b[2] = byte(sessionID >> 8)
	b[3] = byte(sessionID)
	return append(aad, b[:]...)
}

// ---------------------------------------------------------------------------
// 握手路径
// ---------------------------------------------------------------------------

// initiate 向一个已知对端发起 Noise IK 握手。
func (d *datapath) initiate(spec PeerSpec) error {
	uaddr, err := net.ResolveUDPAddr("udp", spec.Addr)
	if err != nil {
		return fmt.Errorf("node: initiate %s: resolve: %w", spec.Addr, err)
	}

	hs, err := handshake.NewWithConfig(handshake.Config{
		Local:     d.local,
		RemotePub: &spec.PublicKey,
		Initiator: true,
		Prologue:  d.cfg.prologue,
	})
	if err != nil {
		return fmt.Errorf("node: initiate %s: %w", spec.Addr, err)
	}

	msg, err := hs.WriteMessage()
	if err != nil {
		return fmt.Errorf("node: initiate %s: write init: %w", spec.Addr, err)
	}

	d.hsMu.Lock()
	d.inflight[uaddr.String()] = &inflight{
		hs:        hs,
		spec:      spec,
		remote:    uaddr,
		startedAt: time.Now(),
	}
	d.hsMu.Unlock()

	if err := d.sendHandshake(packet.TypeHandshake, msg, uaddr); err != nil {
		return err
	}

	d.log.Debug("handshake initiated",
		zap.String("peer", spec.Addr),
		zap.String("peer_id", crypto.NodeIDString(crypto.NodeIDFromPublicKey(spec.PublicKey))),
	)
	return nil
}

// handleHandshake 处理入站握手包。
//
// 分派依据是握手消息自身的类型字节,而不是 ZCPacket.Type:发起方的
// Application-Complete 是 MsgTypeComplete,响应方的 Init 是 MsgTypeInit,
// 二者都以 TypeHandshake 承载。以内部类型为准可以避免 ZCPacket 类型枚举
// 与握手线格式之间的重复映射。
func (d *datapath) handleHandshake(payload []byte, from net.Addr) error {
	msgType, err := handshake.MessageType(payload)
	if err != nil {
		return fmt.Errorf("node: handshake from %s: %w", from, err)
	}

	switch msgType {
	case handshake.MsgTypeInit:
		return d.handleInit(payload, from)
	case handshake.MsgTypeResp:
		return d.handleResp(payload, from)
	case handshake.MsgTypeComplete:
		return d.handleComplete(payload, from)
	default:
		return fmt.Errorf("node: handshake from %s: unknown message type %d", from, msgType)
	}
}

// handleInit 作为响应方处理握手发起消息。
func (d *datapath) handleInit(payload []byte, from net.Addr) error {
	parsed, err := handshake.ParseInit(payload)
	if err != nil {
		return fmt.Errorf("node: handle init from %s: %w", from, err)
	}

	d.hsMu.Lock()
	key := from.String()
	fh := d.inflight[key]
	if fh == nil {
		// 首次收到该地址的 Init:构造响应方握手。
		hs, err := handshake.NewWithConfig(handshake.Config{
			Local:     d.local,
			Initiator: false,
			Prologue:  d.cfg.prologue,
		})
		if err != nil {
			d.hsMu.Unlock()
			return fmt.Errorf("node: new responder: %w", err)
		}
		fh = &inflight{hs: hs, remote: udpAddrOf(from), startedAt: time.Now()}
		d.inflight[key] = fh
	}
	d.byIndex[parsed.SenderIndex] = fh
	d.hsMu.Unlock()

	// ReadMessage 校验 MAC1/时间戳并把对端公钥解出。
	if _, err := fh.hs.ReadMessage(payload); err != nil {
		d.hsMu.Lock()
		delete(d.inflight, key)
		delete(d.byIndex, parsed.SenderIndex)
		d.hsMu.Unlock()
		return fmt.Errorf("node: read init from %s: %w", from, err)
	}

	// 产出 Resp;这是 IK 的最后一条 Noise 消息,密钥在此派生。
	resp, err := fh.hs.WriteMessage()
	if err != nil {
		return fmt.Errorf("node: write resp to %s: %w", from, err)
	}
	if err := d.sendHandshake(packet.TypeHandshakeResp, resp, from); err != nil {
		return err
	}

	d.log.Debug("handshake init processed",
		zap.String("peer", from.String()),
		zap.String("peer_id", crypto.NodeIDString(fh.hs.RemoteNodeID())),
	)
	return nil
}

// handleResp 作为发起方处理握手响应消息。
func (d *datapath) handleResp(payload []byte, from net.Addr) error {
	d.hsMu.Lock()
	fh := d.inflight[from.String()]
	d.hsMu.Unlock()
	if fh == nil {
		return fmt.Errorf("node: handle resp from %s: no inflight handshake", from)
	}

	if _, err := fh.hs.ReadMessage(payload); err != nil {
		return fmt.Errorf("node: read resp from %s: %w", from, err)
	}

	// 产出应用层确认(第 3 条消息)。
	complete, err := fh.hs.WriteMessage()
	if err != nil {
		return fmt.Errorf("node: write complete to %s: %w", from, err)
	}
	if err := d.sendHandshake(packet.TypeHandshake, complete, from); err != nil {
		return err
	}

	return d.adoptHandshake(fh)
}

// handleComplete 作为响应方处理应用层确认消息。
func (d *datapath) handleComplete(payload []byte, from net.Addr) error {
	d.hsMu.Lock()
	fh := d.inflight[from.String()]
	d.hsMu.Unlock()
	if fh == nil {
		return fmt.Errorf("node: handle complete from %s: no inflight handshake", from)
	}

	if _, err := fh.hs.ReadMessage(payload); err != nil {
		return fmt.Errorf("node: read complete from %s: %w", from, err)
	}
	return d.adoptHandshake(fh)
}

// adoptHandshake 把一条已完成的握手转化为会话与 Peer 状态。
func (d *datapath) adoptHandshake(fh *inflight) error {
	if !fh.hs.Complete() {
		return fmt.Errorf("node: adopt handshake: not complete")
	}

	// 先取走会话所有权,再做后续清理:handshake.Close 会关闭其内部持有的
	// 会话,若不先 detach 会把刚加入会话池的会话一并关闭。
	s, err := fh.hs.DetachSession()
	if err != nil {
		return fmt.Errorf("node: adopt handshake: %w", err)
	}

	peerID := fh.hs.RemoteNodeID()

	// 会话池维护"一个对端一个活跃会话"的不变量。
	if _, err := d.sessions.Add(s); err != nil {
		return fmt.Errorf("node: adopt handshake: add session: %w", err)
	}

	// 注册 Peer 并推进状态机。
	p, err := d.peers.GetOrCreate(peerID, fh.remote.String())
	if err != nil {
		return fmt.Errorf("node: adopt handshake: %w", err)
	}
	p.SetAddr(fh.remote.String())
	p.SetSession(s)
	if p.State() == peer.StateIdle || p.State() == peer.StateDead || p.State() == peer.StateFailed {
		if err := p.StartHandshake(); err != nil {
			return err
		}
	}
	if err := p.OnHandshakeSuccess(s); err != nil {
		return err
	}

	// 清理进行中握手。
	d.hsMu.Lock()
	delete(d.inflight, fh.remote.String())
	for idx, item := range d.byIndex {
		if item == fh {
			delete(d.byIndex, idx)
		}
	}
	d.hsMu.Unlock()
	fh.hs.Close()

	d.log.Info("handshake complete",
		zap.String("peer_id", crypto.NodeIDString(peerID)),
		zap.String("peer", fh.remote.String()),
		zap.Uint32("session", s.ID),
	)
	return nil
}

// udpAddrOf 尽力把 net.Addr 归一为 *net.UDPAddr。
func udpAddrOf(addr net.Addr) *net.UDPAddr {
	if ua, ok := addr.(*net.UDPAddr); ok {
		return ua
	}
	ua, err := net.ResolveUDPAddr("udp", addr.String())
	if err != nil {
		return &net.UDPAddr{}
	}
	return ua
}

// ---------------------------------------------------------------------------
// 接收路径
// ---------------------------------------------------------------------------

// run 启动每个 datagram 的读循环、握手超时清理与会话 GC,阻塞至 ctx 结束。
func (d *datapath) run(ctx context.Context) {
	d.mu.Lock()
	dgs := append([]transport.DatagramConn(nil), d.datagrams...)
	d.mu.Unlock()

	var wg sync.WaitGroup
	for _, dg := range dgs {
		wg.Add(1)
		go func(dg transport.DatagramConn) {
			defer wg.Done()
			d.readLoop(ctx, dg)
		}(dg)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		d.maintenance(ctx)
	}()

	<-ctx.Done()
	d.shutdown()
	wg.Wait()
}

// readLoop 从单个 datagram 持续读取并分派。
//
// 缓冲在每次循环内新建:packet.Unmarshal 是零拷贝的,返回的 ZCPacket 直接
// 引用该缓冲,因此必须保证"处理完一个包再读下一个"以及缓冲不被复用。
func (d *datapath) readLoop(ctx context.Context, dg transport.DatagramConn) {
	buf := make([]byte, packet.HeaderSize+packet.DefaultPayloadCap)

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		default:
		}

		// 设置读超时以便及时响应取消。
		_ = dg.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := dg.ReadFrom(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-d.stopCh:
				return
			default:
			}
			d.log.Debug("datagram read error", zap.Error(err))
			return
		}
		if n <= 0 {
			continue
		}

		pkt, err := packet.Unmarshal(buf[:n])
		if err != nil {
			// 非法包静默丢弃:可能来自扫描或其它协议。
			d.log.Debug("drop malformed packet",
				zap.String("from", addr.String()), zap.Error(err))
			continue
		}
		if err := d.dispatch(pkt, addr); err != nil {
			d.log.Debug("dispatch error",
				zap.String("from", addr.String()),
				zap.String("type", packet.TypeName(pkt.Type())),
				zap.Error(err))
		}
	}
}

// dispatch 按 ZCPacket 类型分派一个已解析的包。
func (d *datapath) dispatch(pkt *packet.ZCPacket, from net.Addr) error {
	switch pkt.Type() {
	case packet.TypeHandshake, packet.TypeHandshakeResp:
		return d.handleHandshake(pkt.Payload(), from)

	case packet.TypeData:
		return d.handleData(pkt, from)

	case packet.TypeKeepalive:
		d.handleKeepalive(pkt)

	case packet.TypeClose:
		d.handleClose(pkt)

	default:
		// 其它类型(LSA/Punch/Relay/Probe)属于后续阶段。
		d.log.Debug("ignore packet type",
			zap.String("type", packet.TypeName(pkt.Type())),
			zap.String("from", from.String()))
	}
	return nil
}

// handleData 解密并交付一个数据包。
func (d *datapath) handleData(pkt *packet.ZCPacket, from net.Addr) error {
	sid := pkt.SessionID()
	s, ok := d.sessions.Get(sid)
	if !ok {
		return fmt.Errorf("no session %d", sid)
	}

	srcID := toNodeID(pkt.SrcNodeID())
	dstID := toNodeID(pkt.DstNodeID())

	// 目的地址不是本节点:Phase 1a 尚不转发,直接丢弃。
	if dstID != d.localID {
		return fmt.Errorf("packet addressed to %s, not us", crypto.NodeIDString(dstID))
	}

	aad := buildAAD(srcID, dstID, sid)
	plaintext, err := s.Decrypt(uint64(pkt.Sequence()), pkt.Payload(), aad)
	if err != nil {
		// 解密失败不推进会话状态(由 session.Decrypt 保证),可安全忽略。
		return fmt.Errorf("decrypt: %w", err)
	}

	// 收到有效数据即视为对端存活。
	if p, ok := d.peers.Get(srcID); ok {
		p.OnKeepalive()
	}
	s.Touch()

	if d.onPlaintext != nil {
		// 明文回调必须在返回前完成:readLoop 复用缓冲,回调返回后缓冲即失效。
		d.onPlaintext(srcID, plaintext)
	}
	return nil
}

// handleKeepalive 处理保活包:刷新会话与 Peer 存活时间。
func (d *datapath) handleKeepalive(pkt *packet.ZCPacket) {
	if sid := pkt.SessionID(); sid != 0 {
		if s, ok := d.sessions.Get(sid); ok {
			s.Touch()
		}
	}
	srcID := toNodeID(pkt.SrcNodeID())
	if p, ok := d.peers.Get(srcID); ok {
		p.OnKeepalive()
	}
}

// handleClose 处理关闭包:清理会话与 Peer。
func (d *datapath) handleClose(pkt *packet.ZCPacket) {
	srcID := toNodeID(pkt.SrcNodeID())
	if sid := pkt.SessionID(); sid != 0 {
		d.sessions.Remove(sid)
	}
	d.sessions.RemoveByPeer(srcID)
	d.peers.Remove(srcID)
}

// toNodeID 把 16 字节切片转为 NodeID 数组。长度不足时返回零值。
func toNodeID(b []byte) [crypto.NodeIDSize]byte {
	var id [crypto.NodeIDSize]byte
	copy(id[:], b)
	return id
}

// isTimeout 判断错误是否为超时(含 net.Error 与 transport 包装)。
func isTimeout(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) {
		return nerr.Timeout()
	}
	return false
}

// maintenance 周期执行握手超时清理、会话 GC 与 Peer 超时推进。
func (d *datapath) maintenance(ctx context.Context) {
	ticker := time.NewTicker(peer.KeepaliveInterval)
	defer ticker.Stop()

	// 更细粒度的握手超时检查。
	hsTicker := time.NewTicker(peer.PunchTimeout)
	defer hsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-hsTicker.C:
			d.reapStaleHandshakes()
		case <-ticker.C:
			d.sessions.GC(0)
			d.peers.Tick()
		}
	}
}

// reapStaleHandshakes 清理超时未完成的握手。
func (d *datapath) reapStaleHandshakes() {
	now := time.Now()

	d.hsMu.Lock()
	defer d.hsMu.Unlock()

	for key, fh := range d.inflight {
		if now.Sub(fh.startedAt) < handshake.HandshakeTimeout*time.Duration(handshake.HandshakeMaxRetry) {
			continue
		}
		if fh.hs != nil {
			fh.hs.Close()
		}
		delete(d.inflight, key)
		for idx, item := range d.byIndex {
			if item == fh {
				delete(d.byIndex, idx)
			}
		}
		d.log.Debug("reaped stale handshake", zap.String("peer", key))
	}
}

// shutdown 关闭数据面资源。重复调用安全。
func (d *datapath) shutdown() {
	d.stopOnce.Do(func() {
		close(d.stopCh)
	})

	d.closeMu.Lock()
	d.closed = true
	d.closeMu.Unlock()

	d.hsMu.Lock()
	for _, fh := range d.inflight {
		fh.hs.Close()
	}
	d.inflight = map[string]*inflight{}
	d.byIndex = map[uint32]*inflight{}
	d.hsMu.Unlock()

	_ = d.peers.Close()
	_ = d.sessions.Close()
}
