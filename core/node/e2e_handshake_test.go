package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/session"
	"github.com/LiStudioorg/limesh/core/transport"
)

// handshakePair 是两个已完成握手、可互发加密数据的真实 UDP 节点。
type handshakePair struct {
	A, B       *Node
	keyA, keyB *crypto.KeyPair
	recvA      chan []byte
	recvB      chan []byte
}

// newHandshakeNode 构造一个带密钥与明文回调的测试节点。
func newHandshakeNode(t *testing.T, name string, kp *crypto.KeyPair, recv chan []byte, peers []PeerSpec) *Node {
	t.Helper()

	cfg := config.Default()
	cfg.Network.Name = "e2e-encrypted"
	cfg.Network.Hostname = name
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	require.NoError(t, cfg.Validate())

	n, err := New(Options{
		Config:    cfg,
		Logger:    zap.NewNop(),
		KeyPair:   kp,
		Peers:     peers,
		NoIngress: true, // 本测试只验证加密数据面,不接 TUN
		NoNAT:     true,
		OnPlaintext: func(srcID [crypto.NodeIDSize]byte, plaintext []byte) {
			// 复制明文:回调返回后底层缓冲即失效。
			cp := make([]byte, len(plaintext))
			copy(cp, plaintext)
			select {
			case recv <- cp:
			default:
			}
		},
	})
	require.NoError(t, err)
	return n
}

// startPair 启动 B(被动)与 A(主动向 B 握手),等待双方会话建立。
func startPair(t *testing.T, ctx context.Context) *handshakePair {
	t.Helper()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	recvA := make(chan []byte, 4)
	recvB := make(chan []byte, 4)

	// B 先启动以便获得监听地址。
	nodeB := newHandshakeNode(t, "node-b", keyB, recvB, nil)
	require.NoError(t, nodeB.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeB.Close() })

	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 },
		3*time.Second, 5*time.Millisecond, "B 应开始监听")
	addrB := nodeB.Addrs()[0]

	// A 主动向 B 发起握手。
	nodeA := newHandshakeNode(t, "node-a", keyA, recvA, []PeerSpec{
		{Addr: addrB, PublicKey: keyB.PublicKey},
	})
	require.NoError(t, nodeA.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeA.Close() })

	pair := &handshakePair{A: nodeA, B: nodeB, keyA: keyA, keyB: keyB, recvA: recvA, recvB: recvB}
	return pair
}

// waitSessions 等待双方都建立会话。
func waitSessions(t *testing.T, p *handshakePair) {
	t.Helper()
	require.Eventually(t, func() bool {
		return p.A.Peers().Len() > 0 && p.B.Peers().Len() > 0
	}, 5*time.Second, 10*time.Millisecond, "双方应在超时内完成握手")
}

// TestHandshakeE2E 是 Phase 1a 的核心验收测试。
func TestHandshakeE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p := startPair(t, ctx)
	waitSessions(t, p)

	idA := p.keyA.NodeID()
	idB := p.keyB.NodeID()

	// ---- 双方识别彼此身份 ----
	peerB, ok := p.A.Peers().Get(idB)
	require.True(t, ok, "A 应认识 B 的 NodeID")
	peerA, ok := p.B.Peers().Get(idA)
	require.True(t, ok, "B 应认识 A 的 NodeID")
	assert.Equal(t, crypto.NodeIDString(idA), peerA.NodeIDHex())

	// ---- 会话存在且 ID 一致 ----
	sessA, ok := p.A.Sessions().GetByPeer(idB)
	require.True(t, ok, "A 应有到 B 的会话")
	sessB, ok := p.B.Sessions().GetByPeer(idA)
	require.True(t, ok, "B 应有到 A 的会话")
	assert.Equal(t, sessA.ID, sessB.ID, "两端会话 ID 应一致")

	// ---- 会话 ID 与 DeriveSessionID 独立复算一致 ----
	want := session.DeriveSessionID(p.keyA.PublicKey, p.keyB.PublicKey)
	assert.Equal(t, want, sessA.ID, "会话 ID 应由双方公钥确定性派生")

	assert.Equal(t, peerB.VirtualIP(), crypto.VirtualIPString(idB, true))

	// ---- A → B 加密数据 ----
	require.NoError(t, p.A.SendData(idB, []byte("hello from A")))
	select {
	case got := <-p.recvB:
		assert.Equal(t, "hello from A", string(got))
	case <-time.After(3 * time.Second):
		t.Fatal("B 未在超时内收到 A 的加密数据")
	}

	// ---- B → A 加密数据 ----
	require.NoError(t, p.B.SendData(idA, []byte("hello from B")))
	select {
	case got := <-p.recvA:
		assert.Equal(t, "hello from B", string(got))
	case <-time.After(3 * time.Second):
		t.Fatal("A 未在超时内收到 B 的加密数据")
	}
}

// TestHandshakeTamperedCiphertextRejected 验证篡改密文解密失败,
// 且不推进会话接收状态(不消耗序号)。
func TestHandshakeTamperedCiphertextRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p := startPair(t, ctx)
	waitSessions(t, p)

	idA := p.keyA.NodeID()
	idB := p.keyB.NodeID()
	sessB, ok := p.B.Sessions().GetByPeer(idA)
	require.True(t, ok)

	// 直接用 B 的会话解密一段非法密文应当失败。
	//
	// 说明:Phase 1c 起控制流量(LSA/探测)会占用会话序号,故失败原因可能是
	// 认证失败(ErrDecrypt)或序号已被占用(ErrReplay);两者都证明该伪造包
	// 被拒绝,断言以"被拒绝"为准。
	_, err := sessB.Decrypt(0, []byte("not-a-valid-ciphertext-payload-xx"), buildAAD(idA, idB, sessB.ID))
	require.Error(t, err, "篡改密文必须被拒绝")
	assert.True(t, errors.Is(err, session.ErrDecrypt) || errors.Is(err, session.ErrReplay),
		"应为认证失败或重放拒绝,实际: %v", err)

	// 失败后合法数据仍可到达:序号未被伪造包占用。
	require.NoError(t, p.A.SendData(idB, []byte("legit after tamper")))
	select {
	case got := <-p.recvB:
		assert.Equal(t, "legit after tamper", string(got))
	case <-time.After(3 * time.Second):
		t.Fatal("篡改包不应影响后续合法数据")
	}
}

// TestHandshakeReplayRejected 验证把同一个数据包(相同序号与密文)重放,
// 在会话层会被抗重放窗口拒绝。
//
// 做法:在 A 侧用会话手工加密出一个带明确序号的密文,先在 B 侧用权威序号
// 解密成功,再用同一密文与序号重放进 B 的会话,必须得到 ErrReplay。
func TestHandshakeReplayRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p := startPair(t, ctx)
	waitSessions(t, p)

	idA := p.keyA.NodeID()
	idB := p.keyB.NodeID()

	sessA, ok := p.A.Sessions().GetByPeer(idB)
	require.True(t, ok)
	sessB, ok := p.B.Sessions().GetByPeer(idA)
	require.True(t, ok)
	require.Equal(t, sessA.ID, sessB.ID)

	aad := buildAAD(idA, idB, sessA.ID)

	// A 侧加密出一个具有明确序号的密文。
	seq, ciphertext, err := sessA.SealNext([]byte("replayable"), aad)
	require.NoError(t, err)

	// B 侧第一次解密成功。
	plaintext, err := sessB.Decrypt(seq, ciphertext, aad)
	require.NoError(t, err)
	assert.Equal(t, "replayable", string(plaintext))

	// 用完全相同的 (seq, ciphertext) 重放:B 侧必须拒绝。
	_, err = sessB.Decrypt(seq, ciphertext, aad)
	require.Error(t, err, "重放包必须被拒绝")
	assert.ErrorIs(t, err, session.ErrReplay)
}

// TestHandshakeRemoteNodeIDMatches 验证握手得到的对端身份与配置的公钥派生一致。
func TestHandshakeRemoteNodeIDMatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	p := startPair(t, ctx)
	waitSessions(t, p)

	idA := p.keyA.NodeID()
	idB := p.keyB.NodeID()

	gotB, ok := p.A.Peers().Get(idB)
	require.True(t, ok)
	assert.Equal(t, crypto.NodeIDString(idB), gotB.NodeIDHex())
	assert.Equal(t, crypto.NodeIDFromPublicKey(p.keyB.PublicKey), gotB.NodeID)

	gotA, ok := p.B.Peers().Get(idA)
	require.True(t, ok)
	assert.Equal(t, crypto.NodeIDFromPublicKey(p.keyA.PublicKey), gotA.NodeID)
}

// TestSendDataWithoutSessionFails 验证无会话时发送返回错误而非静默丢弃。
func TestSendDataWithoutSessionFails(t *testing.T) {
	key, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := config.Default()
	cfg.Network.Name = "e2e-encrypted"
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	require.NoError(t, cfg.Validate())

	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: key, NoIngress: true, NoNAT: true})
	require.NoError(t, err)

	other, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	err = n.SendData(other.NodeID(), []byte("nope"))
	require.Error(t, err)
}

// TestNodeWithoutKeyPairHasNoDatapath 验证未配置密钥时节点退化为裸传输,
// 且数据面相关访问器返回零值/nil 而不 panic。
func TestNodeWithoutKeyPairHasNoDatapath(t *testing.T) {
	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Transport: newFakeTransport(), Logger: zap.NewNop()})
	require.NoError(t, err)

	assert.Nil(t, n.Sessions())
	assert.Nil(t, n.Peers())
	assert.Equal(t, [crypto.NodeIDSize]byte{}, n.LocalNodeID())
	assert.ErrorIs(t, n.SendData([crypto.NodeIDSize]byte{}, []byte("x")), ErrNoKeyPair)
}

// TestDatagramAsDatagramInterface 验证 UDP 监听器满足 DatagramConn。
func TestDatagramAsDatagramInterface(t *testing.T) {
	tr := transport.NewUDP(transport.Options{})
	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	_, ok := transport.AsDatagram(ln)
	assert.True(t, ok, "UDP 监听器应支持数据报收发")

	// 非 datagram 的 fakeListener 不应被识别。
	_, ok = transport.AsDatagram(&fakeListener{addr: "x"})
	assert.False(t, ok)
}

// TestDatagramRoundTrip 直接验证数据面用的数据报读写路径。
func TestDatagramRoundTrip(t *testing.T) {
	trA := transport.NewUDP(transport.Options{})
	lnA, err := trA.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer lnA.Close()

	trB := transport.NewUDP(transport.Options{})
	lnB, err := trB.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer lnB.Close()

	dgA, ok := transport.AsDatagram(lnA)
	require.True(t, ok)
	dgB, ok := transport.AsDatagram(lnB)
	require.True(t, ok)

	// B → A
	_, err = dgB.WriteTo([]byte("ping"), lnA.Addr())
	require.NoError(t, err)

	require.NoError(t, dgA.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1500)
	n, from, err := dgA.ReadFrom(buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf[:n]))
	assert.Equal(t, lnB.Addr().String(), from.String())
}
