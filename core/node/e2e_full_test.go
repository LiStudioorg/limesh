//go:build linux && root

package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/api"
	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
)

// 本文件仅在有 root 且启用 root tag 时编译运行。
//
// 它验证 Phase 1d 的完整链路:三个真实 TUN 节点(A-B-C 直线)+ OSPF 收敛
// + Admin API + CLI 命令。CLI 部分通过构建 mesh 二进制并执行子命令完成,
// 从而覆盖"CLI ↔ API"的真实通信路径。

// buildMeshBinary 构建 mesh 可执行文件到临时目录,返回路径。
func buildMeshBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "mesh")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/mesh")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build mesh: %s", out)
	return bin
}

// repoRoot 从测试工作目录回溯到仓库根(含 go.mod)。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	return wd
}

// TestFullStackThreeNodes 启动三个真实 TUN 节点,验证 CLI 与 API。
func TestFullStackThreeNodes(t *testing.T) {
	requireRoot(t)
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("缺少 ip 命令,跳过")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("缺少 go 命令,跳过")
	}

	meshBin := buildMeshBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyC, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	runDir := t.TempDir()
	adminA := filepath.Join(runDir, "a.sock")
	adminB := filepath.Join(runDir, "b.sock")
	adminC := filepath.Join(runDir, "c.sock")

	mkNode := func(name, tun string, kp *crypto.KeyPair, adminSock string, peers []PeerSpec) *Node {
		cfg := tunPingConfig(t, name, "node-"+name, tun)
		cfg.Routing.LSAFloodInterval = time.Second
		cfg.Admin.Enabled = true
		cfg.Admin.Listen = "unix://" + adminSock
		n, err := New(Options{
			Config: cfg, Logger: zap.NewNop(), KeyPair: kp,
			Peers: peers, Version: "test-fullstack",
			Reload: func() error { return nil },
		})
		require.NoError(t, err)
		return n
	}

	// B 为中枢。
	nodeB := mkNode("b", "meshfull1", keyB, adminB, nil)
	defer nodeB.Close()
	require.NoError(t, nodeB.RuntimeStart(ctx))
	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 }, 5*time.Second, 10*time.Millisecond)
	addrB := nodeB.Addrs()[0]

	nodeA := mkNode("a", "meshfull0", keyA, adminA, []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}})
	defer nodeA.Close()
	require.NoError(t, nodeA.RuntimeStart(ctx))

	nodeC := mkNode("c", "meshfull2", keyC, adminC, []PeerSpec{{Addr: addrB, PublicKey: keyB.PublicKey}})
	defer nodeC.Close()
	require.NoError(t, nodeC.RuntimeStart(ctx))

	// 配置虚拟 IP 并启用。
	namer := func(n *Node) string {
		return n.Ingress().(interface{ DeviceName() string }).DeviceName()
	}
	nameA, nameB, nameC := namer(nodeA), namer(nodeB), namer(nodeC)
	t.Cleanup(func() {
		_ = ipCmd("link", "del", nameA)
		_ = ipCmd("link", "del", nameB)
		_ = ipCmd("link", "del", nameC)
	})
	for _, s := range []struct{ name, vip string }{
		{nameA, "10.126.126.1"}, {nameB, "10.126.126.2"}, {nameC, "10.126.126.3"},
	} {
		require.NoError(t, ipCmd("addr", "add", s.vip+"/24", "dev", s.name))
		require.NoError(t, ipCmd("link", "set", s.name, "up"))
	}

	// 等 A 学到到 C 的 2 跳路由。
	require.Eventually(t, func() bool {
		e, ok := nodeA.dp.RouteTo(keyC.NodeID())
		return ok && e.Hops == 2
	}, 20*time.Second, 200*time.Millisecond, "A 应经 B 学到到 C 的路由")

	// ---- CLI 验证:通过 mesh 二进制查询 A 的 Admin API ----
	runMesh := func(args ...string) (string, error) {
		full := append([]string{}, args...)
		full = append(full, "--socket", adminA)
		cmd := exec.CommandContext(ctx, meshBin, full...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// mesh status
	out, err := runMesh("status")
	require.NoError(t, err, "mesh status: %s", out)
	assert.Contains(t, out, "node-a")
	assert.Contains(t, out, "10.126.126.1")

	// mesh peer list
	out, err = runMesh("peer", "list")
	require.NoError(t, err, "mesh peer list: %s", out)
	assert.Contains(t, out, "10.126.126.2")

	// mesh route list
	out, err = runMesh("route", "list")
	require.NoError(t, err, "mesh route list: %s", out)
	assert.Contains(t, out, "10.126.126.3")

	// mesh topo
	out, err = runMesh("topo")
	require.NoError(t, err, "mesh topo: %s", out)
	assert.Contains(t, out, "[self]")

	// mesh config reload
	out, err = runMesh("config", "reload")
	require.NoError(t, err, "mesh config reload: %s", out)

	// ---- ping A -> C ----
	out2, err := exec.Command("ping", "-c", "3", "-W", "3", "10.126.126.3").CombinedOutput()
	t.Logf("A -> C ping:\n%s", out2)
	require.NoError(t, err, "ping 应成功")
	assert.Contains(t, string(out2), "0% packet loss")

	// ---- 停 B,A 到 C 的路由应消失 ----
	require.NoError(t, nodeB.Close())
	require.Eventually(t, func() bool {
		_, ok := nodeA.dp.RouteTo(keyC.NodeID())
		return !ok
	}, 30*time.Second, 200*time.Millisecond, "B 掉线后 A 到 C 的路由应消失")

	_ = adminB
	_ = adminC
}

// TestFullStackAPIOverTCP 验证 TCP + token 模式的 Admin API 可用。
func TestFullStackAPIOverTCP(t *testing.T) {
	requireRoot(t)

	key, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	cfg := config.Default()
	cfg.Network.Name = "tcp-api"
	cfg.Network.Hostname = "node-tcp"
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	cfg.Ingress.Mode = "tun"
	cfg.Ingress.Interface = "meshtcp0"
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "tcp://127.0.0.1:0"
	cfg.Admin.TokenFile = filepath.Join(t.TempDir(), "tok")
	require.NoError(t, cfg.Validate())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	n, err := New(Options{Config: cfg, Logger: zap.NewNop(), KeyPair: key, Version: "tcp-test"})
	if err != nil {
		t.Skipf("无法创建 TUN: %v", err)
	}
	defer n.Close()
	require.NoError(t, n.RuntimeStart(ctx))
	t.Cleanup(func() {
		name := n.Ingress().(interface{ DeviceName() string }).DeviceName()
		_ = ipCmd("link", "del", name)
	})

	require.Eventually(t, func() bool { return n.AdminAddr() != "" }, 3*time.Second, 20*time.Millisecond)

	// 用 token 连接。
	tok, err := os.ReadFile(cfg.Admin.TokenFile)
	require.NoError(t, err)

	cl, err := api.NewClient(api.ClientOptions{
		Listen: "tcp://" + n.AdminAddr(),
		Token:  strings.TrimSpace(string(tok)),
	})
	require.NoError(t, err)

	st, err := cl.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, "node-tcp", st.Hostname)
	assert.Equal(t, "tcp-test", st.Version)

	// 无 token 应失败。
	badCl, err := api.NewClient(api.ClientOptions{
		Listen: "tcp://" + n.AdminAddr(),
		Token:  "wrong-token",
	})
	require.NoError(t, err)
	_, err = badCl.Status(ctx)
	require.Error(t, err, "错误 token 应被拒绝")
}
