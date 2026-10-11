//go:build linux && root

package node

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
)

// 本文件仅在 `go test -tags root` 且以 root 运行时编译与执行。
//
// 它用**真实 TUN 设备**验证 Phase 1b 的验收目标:两个节点通过虚拟网 IP 互 ping。
//
// 运行方式:
//
//	sudo -E env PATH=$PATH LD_LIBRARY_PATH=$LD_LIBRARY_PATH \
//	    go test -race -tags root -run TestTunPing ./core/node/ -v
//
// 无 root 时本文件不参与编译(root tag 未开启),因此 CI 不受影响。

// tunPingConfig 生成一个使用指定 TUN 名与虚拟 IP 的节点配置。
func tunPingConfig(t *testing.T, name, hostname, tunName string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Network.Name = "tun-ping-e2e"
	cfg.Network.Hostname = hostname
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	cfg.Ingress.Mode = "tun"
	cfg.Ingress.Interface = tunName
	// 测试不访问真实 STUN
	cfg.NAT.STUNServers = nil
	require.NoError(t, cfg.Validate())
	return cfg
}

// requireRoot 在非 root 环境下跳过测试。
func requireRoot(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("id", "-u").Output(); err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Skip("需要 root 权限运行 TUN 测试")
	}
}

// ipCmd 执行一条 ip 命令,失败时返回错误。
func ipCmd(args ...string) error {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return nil
}

// TestTunPing 用真实 TUN 设备验证两个节点能通过虚拟 IP 互 ping。
func TestTunPing(t *testing.T) {
	requireRoot(t)

	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("缺少 ip 命令(iproute2),跳过")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	// 用固定虚拟 IP 便于 ping。openTUN 的内部地址由 NodeID 派生,
	// 但 Phase 1b 我们手工配置虚拟 IP 以便 ping 具体地址。
	vipA := "10.126.126.1"
	vipB := "10.126.126.2"
	tunA := "meshping0"
	tunB := "meshping1"

	// ---- 节点 B:先启动(被动响应) ----
	cfgB := tunPingConfig(t, "b", "node-b", tunB)
	nodeB, err := New(Options{Config: cfgB, Logger: zap.NewNop(), KeyPair: keyB})
	require.NoError(t, err)
	defer nodeB.Close()

	require.NoError(t, nodeB.RuntimeStart(ctx))
	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 },
		5*time.Second, 10*time.Millisecond, "B 应开始监听")
	addrB := nodeB.Addrs()[0]

	// ---- 节点 A:主动向 B 握手 ----
	cfgA := tunPingConfig(t, "a", "node-a", tunA)
	nodeA, err := New(Options{
		Config: cfgA, Logger: zap.NewNop(), KeyPair: keyA,
		Peers: []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}},
	})
	require.NoError(t, err)
	defer nodeA.Close()
	require.NoError(t, nodeA.RuntimeStart(ctx))

	// 给 TUN 设备配置虚拟 IP 并启用。
	namer, ok := nodeA.Ingress().(interface{ DeviceName() string })
	require.True(t, ok, "TUN ingress 应暴露 DeviceName")
	nameA := namer.DeviceName()
	nameB := nodeB.Ingress().(interface{ DeviceName() string }).DeviceName()
	t.Cleanup(func() {
		_ = ipCmd("link", "del", nameA)
		_ = ipCmd("link", "del", nameB)
	})

	require.NoError(t, ipCmd("addr", "add", vipA+"/24", "dev", nameA))
	require.NoError(t, ipCmd("link", "set", nameA, "up"))
	require.NoError(t, ipCmd("addr", "add", vipB+"/24", "dev", nameB))
	require.NoError(t, ipCmd("link", "set", nameB, "up"))

	// 等握手完成。
	require.Eventually(t, func() bool {
		return nodeA.Peers().Len() > 0 && nodeB.Peers().Len() > 0
	}, 10*time.Second, 50*time.Millisecond, "双方应完成握手")

	// ---- ping ----
	out, err := exec.Command("ping", "-c", "3", "-W", "3", vipB).CombinedOutput()
	t.Logf("ping 输出:\n%s", out)
	require.NoError(t, err, "ping 应成功")
	assert.Contains(t, string(out), "0% packet loss", "应无丢包")
}

// TestThreeNodeTunLinear 用真实 TUN 设备验证 A-B-C 直线拓扑的透明多跳:
// A 与 C 不直连,A 经 B 中转到 C,ping 通。
func TestThreeNodeTunLinear(t *testing.T) {
	requireRoot(t)

	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("缺少 ip 命令(iproute2),跳过")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyC, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	tunA, tunB, tunC := "meshlin0", "meshlin1", "meshlin2"
	vipA, vipB, vipC := "10.126.126.1", "10.126.126.2", "10.126.126.3"

	// B 为中枢。
	cfgB := tunPingConfig(t, "b", "node-b", tunB)
	cfgB.Routing.LSAFloodInterval = time.Second
	nodeB, err := New(Options{Config: cfgB, Logger: zap.NewNop(), KeyPair: keyB})
	require.NoError(t, err)
	defer nodeB.Close()
	require.NoError(t, nodeB.RuntimeStart(ctx))
	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 },
		5*time.Second, 10*time.Millisecond)
	addrB := nodeB.Addrs()[0]

	// A、C 都只连 B。
	cfgA := tunPingConfig(t, "a", "node-a", tunA)
	cfgA.Routing.LSAFloodInterval = time.Second
	nodeA, err := New(Options{
		Config: cfgA, Logger: zap.NewNop(), KeyPair: keyA,
		Peers: []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}},
	})
	require.NoError(t, err)
	defer nodeA.Close()
	require.NoError(t, nodeA.RuntimeStart(ctx))

	cfgC := tunPingConfig(t, "c", "node-c", tunC)
	cfgC.Routing.LSAFloodInterval = time.Second
	nodeC, err := New(Options{
		Config: cfgC, Logger: zap.NewNop(), KeyPair: keyC,
		Peers: []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}},
	})
	require.NoError(t, err)
	defer nodeC.Close()
	require.NoError(t, nodeC.RuntimeStart(ctx))

	// 配置虚拟 IP。
	namer := func(n *Node) string {
		return n.Ingress().(interface{ DeviceName() string }).DeviceName()
	}
	nameA, nameB, nameC := namer(nodeA), namer(nodeB), namer(nodeC)
	t.Cleanup(func() {
		_ = ipCmd("link", "del", nameA)
		_ = ipCmd("link", "del", nameB)
		_ = ipCmd("link", "del", nameC)
	})
	for _, spec := range []struct{ name, vip string }{
		{nameA, vipA}, {nameB, vipB}, {nameC, vipC},
	} {
		require.NoError(t, ipCmd("addr", "add", spec.vip+"/24", "dev", spec.name))
		require.NoError(t, ipCmd("link", "set", spec.name, "up"))
	}

	// 等 B 与 A、C 握手完成。
	require.Eventually(t, func() bool { return nodeB.Peers().Len() == 2 },
		15*time.Second, 100*time.Millisecond, "B 应与 A、C 完成握手")

	// 等 A 学到到 C 的 2 跳路由。
	require.Eventually(t, func() bool {
		e, ok := nodeA.dp.RouteTo(keyC.NodeID())
		return ok && e.Hops == 2
	}, 15*time.Second, 200*time.Millisecond, "A 应经 B 学到到 C 的路由")

	// A ping C。
	out, err := exec.Command("ping", "-c", "3", "-W", "3", vipC).CombinedOutput()
	t.Logf("A -> C ping 输出:\n%s", out)
	require.NoError(t, err, "A ping C 应成功")
	assert.Contains(t, string(out), "0% packet loss", "应无丢包")
}
