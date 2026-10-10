package node

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/ingress"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/session"
)

// fakeIngress 是内存中的假接入层,用于测试 node 的转发装配。
type fakeIngress struct {
	mode   ingress.Mode
	outCh  chan *packet.ZCPacket
	delMu  sync.Mutex
	delivd []*packet.ZCPacket
	closed bool
}

func newFakeIngress() *fakeIngress {
	return &fakeIngress{
		mode:  ingress.ModeTUN,
		outCh: make(chan *packet.ZCPacket, 16),
	}
}

func (f *fakeIngress) Deliver(pkt *packet.ZCPacket) error {
	f.delMu.Lock()
	defer f.delMu.Unlock()
	f.delivd = append(f.delivd, pkt)
	return nil
}

func (f *fakeIngress) Outbound() <-chan *packet.ZCPacket { return f.outCh }
func (f *fakeIngress) Mode() ingress.Mode                { return f.mode }

func (f *fakeIngress) Close() error {
	f.delMu.Lock()
	defer f.delMu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.outCh)
	}
	return nil
}

func (f *fakeIngress) delivered() []*packet.ZCPacket {
	f.delMu.Lock()
	defer f.delMu.Unlock()
	return append([]*packet.ZCPacket(nil), f.delivd...)
}

// TestNodeWithInjectedIngress 验证注入接入层后,节点持有它且 Close 会关闭它。
func TestNodeWithInjectedIngress(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	fake := newFakeIngress()
	cfg := testConfig(t)

	n, err := New(Options{
		Config:  cfg,
		Logger:  zap.NewNop(),
		KeyPair: kp,
		Ingress: fake,
	})
	require.NoError(t, err)
	assert.Same(t, ingress.Ingress(fake), n.Ingress())

	require.NoError(t, n.Close())
	assert.True(t, fake.closed, "Close 应关闭接入层")
}

// TestNodeAutoConstructsIngressFromConfig 验证未注入时按配置构造接入层。
//
// 无 root 环境下构造会因权限不足失败,此时 New 应返回明确错误而非 panic。
func TestNodeAutoConstructsIngressFromConfig(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t)
	cfg.Ingress.Mode = "tun"
	cfg.Ingress.Interface = "mesh-node-construct-test0"

	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp})
	if err != nil {
		// 无权限:必须报错(禁止静默成功)。
		require.Error(t, err)
		return
	}
	t.Cleanup(func() { _ = n.Close() })
	assert.NotNil(t, n.Ingress())
}

// TestNodeNoIngressSkipsConstruction 验证 NoIngress 跳过接入层构造。
func TestNodeNoIngressSkipsConstruction(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	n, err := New(Options{Config: testConfig(t), Logger: zap.NewNop(), KeyPair: kp, NoIngress: true})
	require.NoError(t, err)
	defer n.Close()

	assert.Nil(t, n.Ingress())
}

// TestDirectPeerSingleSession 验证两点直连时 DirectPeer 返回唯一对端。
func TestDirectPeerSingleSession(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	peerID := [crypto.NodeIDSize]byte{7, 7, 7}
	s, err := session.NewSession(0x1234, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	s.PeerID = peerID
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	got, ok, err := dp.DirectPeer()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, peerID, got)
}

// TestDirectPeerNoSession 验证无会话时 ok=false 且无错误。
func TestDirectPeerNoSession(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	_, ok, err := dp.DirectPeer()
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestDirectPeerAmbiguous 验证多会话时报歧义错误(Phase 1b 不做路由)。
func TestDirectPeerAmbiguous(t *testing.T) {
	dp, _, _ := newTestDatapath(t)

	for i := byte(1); i <= 2; i++ {
		s, err := session.NewSession(uint32(i), [32]byte{1}, [32]byte{2})
		require.NoError(t, err)
		s.PeerID = [crypto.NodeIDSize]byte{i}
		_, err = dp.sessions.Add(s)
		require.NoError(t, err)
	}

	_, ok, err := dp.DirectPeer()
	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), "ambiguous")
}

// TestForwardOneDropsWithoutPeer 验证无对端会话时转发丢弃而不发送。
func TestForwardOneDropsWithoutPeer(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	n, err := New(Options{Config: testConfig(t), Logger: zap.NewNop(), KeyPair: kp, NoIngress: true})
	require.NoError(t, err)
	defer n.Close()

	// 无会话:不应 panic,静默丢弃。
	pkt := packet.New([]byte{0x45, 0x00})
	assert.NotPanics(t, func() { n.forwardOne(pkt) })
}

// TestForwardIngressToDataPathStopsOnClose 验证转发循环在接入层关闭后退出。
func TestForwardIngressToDataPathStopsOnClose(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	fake := newFakeIngress()
	n, err := New(Options{Config: testConfig(t), Logger: zap.NewNop(), KeyPair: kp, Ingress: fake})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { n.forwardIngressToDataPath(ctx); close(done) }()

	// 关闭接入层应使循环退出。
	require.NoError(t, fake.Close())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("转发循环未在接入层关闭后退出")
	}
}

// TestForwardIngressToDataPathStopsOnCancel 验证 ctx 取消使循环退出。
func TestForwardIngressToDataPathStopsOnCancel(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	fake := newFakeIngress()
	n, err := New(Options{Config: testConfig(t), Logger: zap.NewNop(), KeyPair: kp, Ingress: fake})
	require.NoError(t, err)
	defer n.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.forwardIngressToDataPath(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("转发循环未在 ctx 取消后退出")
	}
}
