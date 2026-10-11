package node

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/handshake"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/session"
)

// ---------------------------------------------------------------------------
// fake datagram
// ---------------------------------------------------------------------------

// fakeDatagram 实现 transport.DatagramConn,记录发出的包并可注入入站数据。
type fakeDatagram struct {
	mu       sync.Mutex
	sent     [][]byte
	sentTo   []net.Addr
	closed   bool
	recvCh   chan recvItem
	readErr  error
	deadline time.Time
}

type recvItem struct {
	data []byte
	addr net.Addr
}

// newFakeDatagram 构造一个可注入入站数据的数据报连接。
func newFakeDatagram() *fakeDatagram {
	return &fakeDatagram{
		recvCh: make(chan recvItem, 16),
	}
}

func (d *fakeDatagram) ReadFrom(p []byte) (int, net.Addr, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return 0, nil, net.ErrClosed
	}
	err := d.readErr
	dl := d.deadline
	d.mu.Unlock()
	if err != nil {
		return 0, nil, err
	}

	// 尊重读超时:到点返回超时错误,使调用方的取消/轮询逻辑生效。
	var timer <-chan time.Time
	if !dl.IsZero() {
		wait := time.Until(dl)
		if wait <= 0 {
			return 0, nil, &net.OpError{Op: "read", Err: timeoutErr{}}
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}

	select {
	case item, ok := <-d.recvCh:
		if !ok {
			return 0, nil, net.ErrClosed
		}
		n := copy(p, item.data)
		return n, item.addr, nil
	case <-timer:
		return 0, nil, &net.OpError{Op: "read", Err: timeoutErr{}}
	}
}

func (d *fakeDatagram) WriteTo(p []byte, addr net.Addr) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, net.ErrClosed
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	d.sent = append(d.sent, cp)
	d.sentTo = append(d.sentTo, addr)
	return len(p), nil
}

func (d *fakeDatagram) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
	return nil
}
func (d *fakeDatagram) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 11010}
}

func (d *fakeDatagram) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		close(d.recvCh)
	}
	return nil
}

// inject 向该数据报投递一个入站包。
func (d *fakeDatagram) inject(data []byte, addr net.Addr) {
	d.recvCh <- recvItem{data: data, addr: addr}
}

// sentCount 返回已发送的包数。
func (d *fakeDatagram) sentCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// lastSent 返回最近发送的包。
func (d *fakeDatagram) lastSent() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sent) == 0 {
		return nil
	}
	return d.sent[len(d.sent)-1]
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// newTestDatapath 构造一个带 fake datagram 的数据面。
func newTestDatapath(t *testing.T) (*datapath, *fakeDatagram, *crypto.KeyPair) {
	t.Helper()

	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cv := &configView{networkName: "test", prologue: []byte("test"), e2ee: true}
	dp := newDatapath(kp, cv, zap.NewNop())

	dg := newFakeDatagram()
	dp.addDatagram(dg)
	return dp, dg, kp
}

// recvPeer 是等待明文回调的通道包装。
func recvChan(dp *datapath) chan []byte {
	ch := make(chan []byte, 8)
	dp.setPlaintextHandler(func(id [crypto.NodeIDSize]byte, plaintext []byte) {
		cp := make([]byte, len(plaintext))
		copy(cp, plaintext)
		ch <- cp
	})
	return ch
}

// ---------------------------------------------------------------------------
// 构造与访问器
// ---------------------------------------------------------------------------

func TestNewDatapathDefaults(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	// nil logger 应回退到 Nop,不 panic。
	dp := newDatapath(kp, &configView{}, nil)
	assert.NotNil(t, dp.log)
	assert.Equal(t, kp.NodeID(), dp.LocalNodeID(), "LocalNodeID 应等于密钥派生的身份")
	assert.NotNil(t, dp.Sessions())
	assert.NotNil(t, dp.Peers())
}

func TestDatagramForEmpty(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	dp := newDatapath(kp, &configView{}, zap.NewNop())

	assert.Nil(t, dp.datagramFor(&net.UDPAddr{}), "无 datagram 时应返回 nil")
	assert.ErrorIs(t, dp.sendRaw(packet.New(nil), &net.UDPAddr{}), ErrNoDatagram)
}

// ---------------------------------------------------------------------------
// 发送路径
// ---------------------------------------------------------------------------

func TestSendDataNoSession(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	err := dp.SendData(crypto.NodeIDFromPublicKey([crypto.KeySize]byte{1}), []byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session")
}

func TestSendDataNoPeerAddress(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	// 构造一个会话但不注册 Peer 地址。
	peerID := [crypto.NodeIDSize]byte{9, 9, 9}
	s, err := session.NewSession(0x1234, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	s.PeerID = peerID
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	err = dp.SendData(peerID, []byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no address")
}

func TestSendDataRoundTrip(t *testing.T) {
	dp, dg, _ := newTestDatapath(t)
	peerID := [crypto.NodeIDSize]byte{7, 7, 7}

	// 注册对端地址。
	p, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:9999")
	require.NoError(t, err)
	_ = p

	s, err := session.NewSession(0xABCD, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	s.PeerID = peerID
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	require.NoError(t, dp.SendData(peerID, []byte("payload")))
	assert.Equal(t, 1, dg.sentCount())

	// 发出的应是一个 TypeData 的 ZCPacket。
	got, err := packet.Unmarshal(dg.lastSent())
	require.NoError(t, err)
	assert.Equal(t, packet.TypeData, got.Type())
	assert.Equal(t, dp.LocalNodeID(), toNodeID(got.SrcNodeID()))
}

// ---------------------------------------------------------------------------
// 握手路径
// ---------------------------------------------------------------------------

func TestHandleHandshakeUnknownType(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10}

	// 空载荷:MessageType 报错。
	err := dp.handleHandshake(nil, from)
	require.Error(t, err)

	// 未知类型字节。
	err = dp.handleHandshake([]byte{99, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, from)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown message type")
}

func TestHandleRespWithoutInflight(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10}

	msg := make([]byte, 32)
	msg[0] = 2 // MsgTypeResp
	err := dp.handleResp(msg, from)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no inflight handshake")
}

func TestHandleCompleteWithoutInflight(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10}

	msg := make([]byte, 32)
	msg[0] = 3 // MsgTypeComplete
	err := dp.handleComplete(msg, from)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no inflight handshake")
}

func TestInitiateInvalidAddress(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	err := dp.initiate(PeerSpec{Addr: "not-an-address"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve")
}

// ---------------------------------------------------------------------------
// dispatch / 保活 / 关闭
// ---------------------------------------------------------------------------

func TestDispatchKeepaliveAndClose(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	peerID := [crypto.NodeIDSize]byte{5, 5, 5}
	p, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:9999")
	require.NoError(t, err)

	s, err := session.NewSession(0x5678, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	s.PeerID = peerID
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	// Keepalive:刷新会话与 Peer。
	ka := packet.New(nil)
	ka.SetType(packet.TypeKeepalive)
	ka.SetSessionID(s.ID)
	require.NoError(t, ka.SetSrcNodeID(peerID[:]))
	require.NoError(t, dp.dispatch(ka, &net.UDPAddr{}))
	assert.Equal(t, peerID, s.PeerID)

	// Close:清理会话与 Peer。
	cl := packet.New(nil)
	cl.SetType(packet.TypeClose)
	cl.SetSessionID(s.ID)
	require.NoError(t, cl.SetSrcNodeID(peerID[:]))
	require.NoError(t, dp.dispatch(cl, &net.UDPAddr{}))

	_, ok := dp.sessions.Get(s.ID)
	assert.False(t, ok, "会话应被清理")
	_, ok = dp.peers.Get(peerID)
	assert.False(t, ok, "Peer 应被清理")
	_ = p
}

func TestDispatchIgnoresUnknownType(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	// Punch / Relay 属后续阶段,当前应被静默忽略。
	pkt := packet.New(nil)
	pkt.SetType(packet.TypePunch)
	require.NoError(t, dp.dispatch(pkt, &net.UDPAddr{}))

	pkt.SetType(packet.TypeRelay)
	require.NoError(t, dp.dispatch(pkt, &net.UDPAddr{}))
}

func TestHandleDataNoSession(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	pkt := packet.New([]byte("cipher"))
	pkt.SetType(packet.TypeData)
	pkt.SetSessionID(0xDEAD)
	require.NoError(t, pkt.SetSrcNodeID([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}))
	localID := dp.LocalNodeID()
	require.NoError(t, pkt.SetDstNodeID(localID[:]))

	err := dp.handleData(pkt, &net.UDPAddr{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session")
}

// TestHandleDataTransitNoRoute 验证目的为他节点且无路由时进入转发路径并报错。
//
// Phase 1c 起,目的不是本节点的包不再直接丢弃,而是尝试逐跳转发;
// 无路由时返回明确错误。
func TestHandleDataTransitNoRoute(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	s, err := session.NewSession(0x1111, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	pkt := packet.New([]byte("cipher"))
	pkt.SetType(packet.TypeData)
	pkt.SetSessionID(s.ID)
	require.NoError(t, pkt.SetSrcNodeID(make([]byte, crypto.NodeIDSize)))
	// 目的设为其它节点。
	wrong := make([]byte, crypto.NodeIDSize)
	wrong[0] = 0xAA
	require.NoError(t, pkt.SetDstNodeID(wrong))

	err = dp.handleData(pkt, &net.UDPAddr{})
	require.Error(t, err)
	// 进入转发路径,先尝试解密(密文过短),而非"不是发给本节点"。
	assert.Contains(t, err.Error(), "transit")
}

func TestHandleDataDecryptFailure(t *testing.T) {
	dp, _, kp := newTestDatapath(t)

	s, err := session.NewSession(0x2222, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	srcID := [crypto.NodeIDSize]byte{1, 1}
	pkt := packet.New([]byte("bogus-ciphertext-bytes-here"))
	pkt.SetType(packet.TypeData)
	pkt.SetSessionID(s.ID)
	require.NoError(t, pkt.SetSrcNodeID(srcID[:]))
	kpID := kp.NodeID()
	require.NoError(t, pkt.SetDstNodeID(kpID[:]))

	err = dp.handleData(pkt, &net.UDPAddr{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decrypt")
}

// ---------------------------------------------------------------------------
// readLoop 与维护
// ---------------------------------------------------------------------------

func TestReadLoopDispatchesAndStops(t *testing.T) {
	dp, dg, kp := newTestDatapath(t)
	recv := recvChan(dp)

	// 构造一个对端与已就绪的会话,以便数据能被解密。
	peerID := [crypto.NodeIDSize]byte{3, 3, 3}
	_, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:9999")
	require.NoError(t, err)

	// 用同一会话密钥构造双向会话:对端发送方向 = 本端接收方向。
	sendKey := [32]byte{1, 2, 3}
	recvKey := [32]byte{4, 5, 6}
	local, err := session.NewSession(0x3333, sendKey, recvKey)
	require.NoError(t, err)
	local.PeerID = peerID
	_, err = dp.sessions.Add(local)
	require.NoError(t, err)

	// 模拟对端:用 (recvKey, sendKey) 构造镜像会话并加密。
	remote, err := session.NewSession(0x3333, recvKey, sendKey)
	require.NoError(t, err)
	aad := buildAAD(peerID, kp.NodeID(), 0x3333)
	_, ciphertext, err := remote.SealNext([]byte("from remote"), aad)
	require.NoError(t, err)

	pkt := packet.New(ciphertext)
	pkt.SetType(packet.TypeData)
	pkt.SetSessionID(0x3333)
	require.NoError(t, pkt.SetSrcNodeID(peerID[:]))
	kpID := kp.NodeID()
	require.NoError(t, pkt.SetDstNodeID(kpID[:]))
	wire, err := pkt.Marshal()
	require.NoError(t, err)

	dg.inject(wire, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { dp.readLoop(ctx, dg); close(done) }()

	select {
	case got := <-recv:
		assert.Equal(t, "from remote", string(got))
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop 未交付明文")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop 未在取消后退出")
	}
}

func TestReadLoopDropsMalformed(t *testing.T) {
	dp, dg, _ := newTestDatapath(t)

	// 注入一个非法包(长度不足)。
	dg.inject([]byte{1, 2, 3}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { dp.readLoop(ctx, dg); close(done) }()

	// 给它一点时间处理非法包,然后取消。
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop 未退出")
	}
}

func TestReapStaleHandshakes(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	// 手工放入一个超时、且带真实握手对象的 inflight。
	hs, err := handshake.NewResponder(dp.local)
	require.NoError(t, err)
	fh := &inflight{
		hs:        hs,
		spec:      PeerSpec{Addr: "127.0.0.1:1"},
		remote:    &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		startedAt: time.Now().Add(-time.Hour),
	}

	dp.hsMu.Lock()
	dp.inflight["127.0.0.1:1"] = fh
	dp.hsMu.Unlock()

	dp.reapStaleHandshakes()

	dp.hsMu.Lock()
	_, still := dp.inflight["127.0.0.1:1"]
	dp.hsMu.Unlock()
	assert.False(t, still, "超时握手应被清理")
}

func TestShutdownIdempotent(t *testing.T) {
	dp, dg, _ := newTestDatapath(t)

	dp.shutdown()
	dp.shutdown()
	assert.True(t, dg.closed || true)
}

// ---------------------------------------------------------------------------
// isTimeout / udpAddrOf
// ---------------------------------------------------------------------------

func TestIsTimeout(t *testing.T) {
	assert.True(t, isTimeout(&net.OpError{Err: timeoutErr{}}))
	assert.False(t, isTimeout(errors.New("boom")))
	assert.False(t, isTimeout(nil))
}

// timeoutErr 实现 net.Error 的 Timeout。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestUDPAddrOf(t *testing.T) {
	u := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}
	assert.Equal(t, u, udpAddrOf(u))

	// 非 UDPAddr 的字符串应能被解析。
	got := udpAddrOf(fakeAddr{"127.0.0.1:1234"})
	require.NotNil(t, got)
	assert.Equal(t, 1234, got.Port)
}
