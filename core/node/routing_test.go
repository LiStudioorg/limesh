package node

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/peer"
	"github.com/LiStudioorg/limesh/core/route"
	"github.com/LiStudioorg/limesh/core/session"
)

// routedTestDatapath 构造一个带路由表的数据面,用于路由相关单测。
func routedTestDatapath(t *testing.T) (*datapath, [crypto.NodeIDSize]byte) {
	t.Helper()
	dp, _, kp := newTestDatapath(t)
	return dp, kp.NodeID()
}

// TestBuildSelfLSANeighborCost 验证 LSA 邻居开销来自探测采样。
func TestBuildSelfLSANeighborCost(t *testing.T) {
	dp, self := routedTestDatapath(t)

	peerID := [crypto.NodeIDSize]byte{2}
	p, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:1")
	require.NoError(t, err)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	lsa := dp.buildSelfLSA()
	assert.Equal(t, self, lsa.Origin)
	require.Len(t, lsa.Neighbors, 1)
	assert.Equal(t, peerID, lsa.Neighbors[0].NodeID)
	assert.Equal(t, uint32(route.HopCost), lsa.Neighbors[0].Cost, "无采样时用固定跳开销")

	// 拓扑库应包含本节点 LSA。
	_, ok := dp.topology.Get(self)
	assert.True(t, ok, "本节点 LSA 应进入自己的拓扑库")
}

// TestBuildSelfLSASkipsNonDirect 验证非 Direct/Relayed 的 Peer 不写入 LSA。
func TestBuildSelfLSASkipsNonDirect(t *testing.T) {
	dp, _ := routedTestDatapath(t)

	idle, err := dp.peers.GetOrCreate([crypto.NodeIDSize]byte{3}, "127.0.0.1:2")
	require.NoError(t, err)
	assert.Equal(t, peer.StateIdle, idle.State())

	lsa := dp.buildSelfLSA()
	assert.Empty(t, lsa.Neighbors, "Idle 邻居不应写入 LSA")
}

// TestSendToNodeSelf 验证发给自身的包被拒绝。
func TestSendToNodeSelf(t *testing.T) {
	dp, self := routedTestDatapath(t)
	err := dp.SendToNode(self, []byte{0x45})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "self")
}

// TestSendToNodeNoRoute 验证无路由时报错。
func TestSendToNodeNoRoute(t *testing.T) {
	dp, _ := routedTestDatapath(t)
	err := dp.SendToNode([crypto.NodeIDSize]byte{0xAB}, []byte{0x45})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no route")
}

// TestResolveDstNodeIDSelf 验证目的虚拟 IP 为本节点时解析成功。
func TestResolveDstNodeIDSelf(t *testing.T) {
	dp, self := routedTestDatapath(t)

	vip := crypto.DeriveVirtualIP(self, true).As4()
	ip := ipv4Packet(vip, vip, nil)

	got, err := dp.resolveDstNodeID(ip)
	require.NoError(t, err)
	assert.Equal(t, self, got)
}

// TestResolveDstNodeIDPeer 验证能解析出直接邻居的虚拟 IP。
func TestResolveDstNodeIDPeer(t *testing.T) {
	dp, _ := routedTestDatapath(t)

	peerID := [crypto.NodeIDSize]byte{9, 8, 7}
	_, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:3")
	require.NoError(t, err)

	vip := crypto.DeriveVirtualIP(peerID, true).As4()
	ip := ipv4Packet(vip, vip, nil)

	got, err := dp.resolveDstNodeID(ip)
	require.NoError(t, err)
	assert.Equal(t, peerID, got)
}

// TestResolveDstNodeIDUnknown 验证未知虚拟 IP 报错。
func TestResolveDstNodeIDUnknown(t *testing.T) {
	dp, _ := routedTestDatapath(t)
	ip := ipv4Packet([4]byte{10, 126, 126, 200}, [4]byte{10, 126, 126, 201}, nil)
	_, err := dp.resolveDstNodeID(ip)
	require.Error(t, err)
}

// TestDstIPv4Errors 覆盖 IP 头解析的边界。
func TestDstIPv4Errors(t *testing.T) {
	_, err := dstIPv4([]byte{0x45})
	require.Error(t, err)

	// 非 IPv4(version 6)。
	v6 := make([]byte, 40)
	v6[0] = 0x60
	_, err = dstIPv4(v6)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ipv4")
}

// TestForwardTransitNoRoute 验证转发时无路由报错。
func TestForwardTransitNoRoute(t *testing.T) {
	dp, _ := routedTestDatapath(t)

	s, err := session.NewSession(0x1, [32]byte{1}, [32]byte{2})
	require.NoError(t, err)
	_, err = dp.sessions.Add(s)
	require.NoError(t, err)

	err = dp.forwardTransit(s.ID, 0, [crypto.NodeIDSize]byte{1}, [crypto.NodeIDSize]byte{2}, []byte("x"))
	require.Error(t, err)
}

// TestSendControlNoSession 验证无会话时控制包发送失败。
func TestSendControlNoSession(t *testing.T) {
	dp, _ := routedTestDatapath(t)
	err := dp.sendControl([crypto.NodeIDSize]byte{1}, packet.TypeLSA, []byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session")
}

// TestLSAForwardDedup 验证同一序号 LSA 只转发一次。
func TestLSAForwardDedup(t *testing.T) {
	dp, _ := routedTestDatapath(t)

	origin := [crypto.NodeIDSize]byte{4}
	lsa := route.NewLSA(origin)
	lsa.SeqNo = 5

	// 第一次:登记 seen。
	dp.forwardLSA([crypto.NodeIDSize]byte{1}, lsa)
	dp.lsaMu.Lock()
	seen := dp.seenLSA[origin]
	dp.lsaMu.Unlock()
	assert.Equal(t, uint32(5), seen)

	// 更旧的序号:不更新 seen。
	older := route.NewLSA(origin)
	older.SeqNo = 3
	dp.forwardLSA([crypto.NodeIDSize]byte{1}, older)
	dp.lsaMu.Lock()
	seen = dp.seenLSA[origin]
	dp.lsaMu.Unlock()
	assert.Equal(t, uint32(5), seen, "旧序号不应覆盖 seen")
}

// TestRecomputeRoutesAtomic 验证重算后路由表可原子读取。
func TestRecomputeRoutesAtomic(t *testing.T) {
	dp, self := routedTestDatapath(t)

	// 构造 A(=self) - peer 的双向 LSA。
	peerID := [crypto.NodeIDSize]byte{6, 6}
	selfLSA := route.NewLSA(self)
	selfLSA.SeqNo = 1
	selfLSA.AddNeighbor(peerID, 10)
	dp.topology.Update(selfLSA)

	peerLSA := route.NewLSA(peerID)
	peerLSA.SeqNo = 1
	peerLSA.AddNeighbor(self, 10)
	dp.topology.Update(peerLSA)

	dp.recomputeRoutes()
	e, ok := dp.RouteTo(peerID)
	require.True(t, ok)
	assert.Equal(t, peerID, e.NextHop)
	assert.Equal(t, uint8(1), e.Hops)
	assert.NotNil(t, dp.RouteTable())
	assert.NotNil(t, dp.Topology())
}

// TestAdminSnapshotMethods 验证 Node 的 api.NodeInfo 快照转换。
func TestAdminSnapshotMethods(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true, NoAdmin: true, Version: "vtest"})
	require.NoError(t, err)
	defer n.Close()

	// Status 基本字段。
	st := n.Status()
	assert.Equal(t, kp.NodeIDHex(), st.NodeID)
	assert.Equal(t, "vtest", st.Version)
	assert.NotEmpty(t, st.VirtualIP)

	// 空快照应返回非 nil 切片。
	assert.NotNil(t, n.PeerList())
	assert.NotNil(t, n.RouteList())
	topo := n.TopologySnapshot()
	assert.NotNil(t, topo.Nodes)
	assert.NotNil(t, topo.Edges)
}

// TestAdminSnapshotWithRoute 验证路由快照包含已计算条目。
func TestAdminSnapshotWithRoute(t *testing.T) {
	dp, self := routedTestDatapath(t)

	peerID := [crypto.NodeIDSize]byte{6, 6}
	selfLSA := route.NewLSA(self)
	selfLSA.SeqNo = 1
	selfLSA.AddNeighbor(peerID, 10)
	dp.topology.Update(selfLSA)
	peerLSA := route.NewLSA(peerID)
	peerLSA.SeqNo = 1
	peerLSA.AddNeighbor(self, 10)
	dp.topology.Update(peerLSA)
	dp.recomputeRoutes()

	// 构造一个 Node 包装该数据面以测试快照方法。
	n := &Node{dp: dp, startedAt: time.Now(), Hostname: "x", version: "v"}
	routes := n.RouteList()
	require.Len(t, routes, 2)
	topo := n.TopologySnapshot()
	assert.Len(t, topo.Nodes, 2)
	assert.Len(t, topo.Edges, 2)
}

// TestAdminStartDisabled 验证未启用时 Admin API 不启动。
func TestAdminStartDisabled(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true})
	require.NoError(t, err)
	defer n.Close()

	// 默认 admin.enabled=false。
	assert.Nil(t, n.startAdminAPI(context.Background()))
	assert.Empty(t, n.AdminAddr())
}

// TestAdminStartEnabled 验证启用时 API 监听并可在停止后清理。
func TestAdminStartEnabled(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")

	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	cfg := testConfig(t)
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "unix://" + sock

	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true, Version: "v1"})
	require.NoError(t, err)
	defer n.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := n.startAdminAPI(ctx)
	require.NotNil(t, done)
	require.Eventually(t, func() bool { return n.AdminAddr() != "" }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, sock, n.AdminAddr())

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("admin server 未在取消后退出")
	}
}

// TestPeerListWithData 验证 PeerList 快照包含对端。
func TestPeerListWithData(t *testing.T) {
	dp, _, _ := newTestDatapath(t)
	peerID := [crypto.NodeIDSize]byte{3, 4, 5}
	p, err := dp.peers.GetOrCreate(peerID, "127.0.0.1:9999")
	require.NoError(t, err)
	require.NoError(t, p.StartHandshake())
	require.NoError(t, p.OnHandshakeSuccess(nil))
	require.NoError(t, p.OnPunchSuccess())

	n := &Node{dp: dp, startedAt: time.Now(), Hostname: "h"}
	peers := n.PeerList()
	require.Len(t, peers, 1)
	assert.Equal(t, crypto.NodeIDString(peerID), peers[0].NodeID)
	assert.Equal(t, "direct", peers[0].State)

	st := n.Status()
	assert.Equal(t, 1, st.PeerCount)
	assert.Equal(t, 1, st.DirectPeers)
}
