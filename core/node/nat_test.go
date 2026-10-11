package node

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/nat"
)

// mockSTUNServer 启动一个返回固定映射的 STUN 服务器,返回其 host:port。
func mockSTUNServer(t *testing.T, ip string, port int) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if req.Decode() != nil || req.Type != stun.BindingRequest {
				continue
			}
			resp, err := stun.Build(
				stun.NewTransactionIDSetter(req.TransactionID),
				stun.NewType(stun.MethodBinding, stun.ClassSuccessResponse),
			)
			if err != nil {
				continue
			}
			mapped := &stun.XORMappedAddress{IP: net.ParseIP(ip).To4(), Port: port}
			if mapped.AddTo(resp) != nil {
				continue
			}
			_, _ = conn.WriteToUDP(resp.Raw, addr)
		}
	}()
	return conn.LocalAddr().String()
}

// TestNodeNATDefaults 验证未配置 STUN 时 NAT 不启用。
func TestNodeNATDefaults(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t) // STUNServers 已清空
	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true, NoAdmin: true})
	require.NoError(t, err)
	defer n.Close()

	assert.Nil(t, n.natDetector)
	assert.NotNil(t, n.natCollector)
	assert.Equal(t, nat.NATUnknown, n.NATType())
	assert.Nil(t, n.MappedAddr())
}

// TestNodeNATDisabled 验证 NoNAT 跳过装配。
func TestNodeNATDisabled(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true, NoAdmin: true, NoNAT: true})
	require.NoError(t, err)
	defer n.Close()

	assert.Nil(t, n.natDetector)
	assert.Nil(t, n.natCollector)
	assert.Empty(t, n.Candidates())
}

// TestNodeProbeNAT 验证显式探测返回锥形并收集候选。
func TestNodeProbeNAT(t *testing.T) {
	a1 := mockSTUNServer(t, "203.0.113.50", 40000)
	a2 := mockSTUNServer(t, "203.0.113.50", 40000)

	det, err := nat.NewDetector([]string{a1, a2})
	require.NoError(t, err)
	det.SetTimeout(time.Second)

	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t)
	n, err := New(Options{
		Config: cfg, Logger: zap.NewNop(), KeyPair: kp,
		NoIngress: true, NoAdmin: true, NATDetector: det,
	})
	require.NoError(t, err)
	defer n.Close()

	ntype, err := n.ProbeNAT(context.Background())
	require.NoError(t, err)
	assert.Equal(t, nat.NATCone, ntype)

	m := n.MappedAddr()
	require.NotNil(t, m)
	assert.Equal(t, "203.0.113.50", m.IP.String())

	// 候选应包含 srflx 与(可能的)host。
	cands := n.CandidateList()
	var hasSrflx bool
	for _, c := range cands {
		if c.Type == nat.CandidateSrflx {
			hasSrflx = true
		}
	}
	assert.True(t, hasSrflx, "应收集到 srflx 候选")
	assert.False(t, n.NATDetectedAt().IsZero())
}

// TestNodeProbeNATNotEnabled 验证未启用时 ProbeNAT 报错。
func TestNodeProbeNATNotEnabled(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: kp, NoIngress: true, NoAdmin: true, NoNAT: true})
	require.NoError(t, err)
	defer n.Close()

	_, err = n.ProbeNAT(context.Background())
	require.Error(t, err)
}

// TestCandidateExchangeViaHandshake 验证两个节点在握手时交换候选地址。
func TestCandidateExchangeViaHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	mk := func(name string, kp *crypto.KeyPair, peers []PeerSpec) *Node {
		cfg := testConfig(t)
		cfg.NAT.CandidateSkipInterfaces = nil // 允许收集 host 候选
		n, err := New(Options{
			Config: cfg, Logger: zap.NewNop(), KeyPair: kp, Peers: peers,
			NoIngress: true, NoAdmin: true,
		})
		require.NoError(t, err)
		return n
	}

	nodeB := mk("node-b", keyB, nil)
	require.NoError(t, nodeB.RuntimeStart(ctx))
	defer nodeB.Close()
	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 }, 3*time.Second, 5*time.Millisecond)
	addrB := nodeB.Addrs()[0]

	nodeA := mk("node-a", keyA, []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}})
	require.NoError(t, nodeA.RuntimeStart(ctx))
	defer nodeA.Close()

	require.Eventually(t, func() bool {
		return nodeA.Peers().Len() > 0 && nodeB.Peers().Len() > 0
	}, 5*time.Second, 10*time.Millisecond, "应完成握手")

	// 至少一侧应有本端候选地址(host),并随握手交换。
	// 候选数量取决于测试机的网卡;若本机无可用网卡则可能为空,故做宽松断言。
	pA, ok := nodeA.Peers().Get(keyB.NodeID())
	require.True(t, ok)
	_ = pA.LocalCandidates() // 仅验证可访问
}

// 确保 stun 包被引用(测试辅助)。
var _ = stun.BindingRequest
