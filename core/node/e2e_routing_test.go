package node

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
)

// TestThreeNodeRoutingLoopback 是无 root 环境下的三节点直线拓扑验收测试。
//
// 拓扑:A — B — C(A 与 C 不直接相连)。
// 用假 TUN 设备模拟三台机器,验证:
//
//	A.TUN 写包 → A 查路由(下一跳 B) → 加密发给 B → B 解密 → 查路由
//	  (下一跳 C) → 重新加密发给 C → C 解密 → 写入 C.TUN
//
// 即 A 经 B 中转到 C 的透明多跳转发。
func TestThreeNodeRoutingLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyC, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	// 缩短泛洪周期以加快收敛。
	fast := func(c *config.Config) {
		c.Routing.LSAFloodInterval = 300 * time.Millisecond
	}

	// B 是中心:A 与 C 都只认识 B。
	nodeB := newIngressNodeWithRouting(t, "node-b", keyB, nil, fast)
	require.NoError(t, nodeB.node.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeB.node.Close() })
	require.Eventually(t, func() bool { return len(nodeB.node.Addrs()) == 1 },
		3*time.Second, 5*time.Millisecond, "B 应开始监听")
	addrB := nodeB.node.Addrs()[0]

	// A 连 B。
	nodeA := newIngressNodeWithRouting(t, "node-a", keyA, []PeerSpec{
		{Addr: addrB, PublicKey: keyB.PublicKey},
	}, fast)
	require.NoError(t, nodeA.node.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeA.node.Close() })

	// C 连 B。
	nodeC := newIngressNodeWithRouting(t, "node-c", keyC, []PeerSpec{
		{Addr: addrB, PublicKey: keyB.PublicKey},
	}, fast)
	require.NoError(t, nodeC.node.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeC.node.Close() })

	idA := keyA.NodeID()
	idB := keyB.NodeID()
	idC := keyC.NodeID()

	// 等 B 与 A、C 都完成握手。
	require.Eventually(t, func() bool {
		return nodeB.node.Peers().Len() == 2
	}, 8*time.Second, 10*time.Millisecond, "B 应与 A、C 均完成握手")

	// 等路由收敛:A 应学到到 C 的路由,下一跳是 B。
	require.Eventually(t, func() bool {
		e, ok := nodeA.node.dp.RouteTo(idC)
		return ok && e.NextHop == idB && e.Hops == 2
	}, 8*time.Second, 20*time.Millisecond, "A 应经 B 学到到 C 的 2 跳路由")

	// 同理 C 到 A(等待独立收敛)。
	require.Eventually(t, func() bool {
		e, ok := nodeC.node.dp.RouteTo(idA)
		return ok && e.NextHop == idB && e.Hops == 2
	}, 8*time.Second, 20*time.Millisecond, "C 应经 B 学到到 A 的 2 跳路由")

	// 端到端:构造 A → C 的 IP 包,写入 A 的 TUN。
	vipA := crypto.DeriveVirtualIP(idA, true).As4()
	vipC := crypto.DeriveVirtualIP(idC, true).As4()
	ipPacket := ipv4Packet(vipA, vipC, []byte{0x08, 0x00, 0x00, 0x00, 0x12, 0x34, 0x00, 0x01})
	nodeA.dev.Inbound <- ipPacket

	select {
	case got := <-nodeC.dev.Outbound:
		assert.Equal(t, ipPacket, got, "C 应经 B 中转收到 A 的原始 IP 包")
	case <-time.After(8 * time.Second):
		t.Fatal("C 未在超时内收到 A 经 B 中转的 IP 包")
	}

	// 反向 C → A。
	reply := ipv4Packet(vipC, vipA, []byte{0x08, 0x00, 0x00, 0x00, 0x12, 0x34, 0x00, 0x02})
	nodeC.dev.Inbound <- reply
	select {
	case got := <-nodeA.dev.Outbound:
		assert.Equal(t, reply, got, "A 应经 B 中转收到 C 的原始 IP 包")
	case <-time.After(8 * time.Second):
		t.Fatal("A 未在超时内收到 C 经 B 中转的 IP 包")
	}
}

// TestThreeNodeFailover 验证中间节点 B 掉线后,A 到 C 的路由消失。
//
// 由于 A 与 C 之间没有直连,失去 B 后两者应变为不可达:路由表条目被
// 老化清除,A 的拓扑不再包含 C。
func TestThreeNodeFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyC, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	// 缩短老化与泛洪周期,使故障收敛在测试可接受的时间内发生。
	fast := func(c *config.Config) {
		c.Routing.LSAMaxAge = 3 * time.Second
		c.Routing.LSAFloodInterval = 300 * time.Millisecond
	}

	nodeB := newIngressNodeWithRouting(t, "node-b", keyB, nil, fast)
	require.NoError(t, nodeB.node.RuntimeStart(ctx))
	require.Eventually(t, func() bool { return len(nodeB.node.Addrs()) == 1 },
		3*time.Second, 5*time.Millisecond)
	addrB := nodeB.node.Addrs()[0]

	nodeA := newIngressNodeWithRouting(t, "node-a", keyA,
		[]PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}}, fast)
	require.NoError(t, nodeA.node.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeA.node.Close() })

	nodeC := newIngressNodeWithRouting(t, "node-c", keyC,
		[]PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}}, fast)
	require.NoError(t, nodeC.node.RuntimeStart(ctx))
	t.Cleanup(func() { _ = nodeC.node.Close() })

	idC := keyC.NodeID()

	// 收敛:A 能到 C。
	require.Eventually(t, func() bool {
		_, ok := nodeA.node.dp.RouteTo(idC)
		return ok
	}, 8*time.Second, 20*time.Millisecond, "A 应先收敛出到 C 的路由")

	// 停掉 B。
	require.NoError(t, nodeB.node.Close())

	// A 到 C 的路由应最终消失。
	require.Eventually(t, func() bool {
		_, ok := nodeA.node.dp.RouteTo(idC)
		return !ok
	}, 30*time.Second, 100*time.Millisecond, "B 掉线后 A 到 C 的路由应消失")
}
