package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/api"
	"github.com/LiStudioorg/limesh/core/config"
)

// cliMockNode 是 api.NodeInfo 的内存实现,供 CLI 测试使用。
type cliMockNode struct {
	cfg *config.Config
}

func (m *cliMockNode) Status() api.StatusSnapshot {
	return api.StatusSnapshot{
		NodeID:       "aabbccdd11223344",
		Hostname:     "node-a",
		VirtualIP:    "10.126.126.1",
		PeerCount:    2,
		DirectPeers:  1,
		RelayedPeers: 1,
		RouteCount:   3,
		Version:      "test",
	}
}

func (m *cliMockNode) PeerList() []api.PeerSnapshot {
	return []api.PeerSnapshot{
		{NodeID: "1122334455667788", VirtualIP: "10.126.126.2", State: "Direct", Addr: "127.0.0.1:11010", RTTMillis: 2.0},
		{NodeID: "99aabbccddeeff00", VirtualIP: "10.126.126.3", State: "Relayed", Addr: "10.0.0.9:11010", RTTMillis: 45.0, LossRate: 0.002},
	}
}

func (m *cliMockNode) RouteList() []api.RouteSnapshot {
	return []api.RouteSnapshot{
		{Dst: "1122334455667788", DstVirtualIP: "10.126.126.2", NextHop: "1122334455667788", Cost: 12, Hops: 1},
		{Dst: "99aabbccddeeff00", DstVirtualIP: "10.126.126.3", NextHop: "1122334455667788", Cost: 22, Hops: 2},
	}
}

func (m *cliMockNode) TopologySnapshot() api.TopologySnapshot {
	return api.TopologySnapshot{
		Nodes: []api.NodeSnapshot{
			{NodeID: "aabbccdd11223344", VirtualIP: "10.126.126.1", IsSelf: true},
			{NodeID: "1122334455667788", VirtualIP: "10.126.126.2"},
		},
		Edges: []api.EdgeSnapshot{
			{From: "aabbccdd11223344", To: "1122334455667788", Cost: 12},
		},
	}
}

func (m *cliMockNode) Config() *config.Config { return m.cfg }

func (m *cliMockNode) NAT() api.NATSnapshot {
	return api.NATSnapshot{Type: "cone", Mapped: "203.0.113.5:45231", CandidateCount: 2}
}

func (m *cliMockNode) Candidates() []api.CandidateSnapshot {
	return []api.CandidateSnapshot{
		{Type: "host", Addr: "192.168.1.10:11010", Priority: 2130706431, Source: "eth0"},
		{Type: "host", Addr: "[2001:db8::1]:11010", Priority: 2130706687, Source: "eth0"},
		{Type: "srflx", Addr: "203.0.113.5:45231", Priority: 16777215, Source: "stun:stun.l.google.com:19302"},
	}
}

func (m *cliMockNode) ProbeNAT(ctx context.Context) error { return nil }

// startMockAPI 启动一个基于 Unix Socket 的 mock Admin API,返回 socket 路径。
func startMockAPI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")

	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "unix://" + sock

	srv, err := api.NewServer(api.ServerOptions{
		Config:  cfg,
		Node:    &cliMockNode{cfg: cfg},
		Logger:  zap.NewNop(),
		Version: "test-version",
		Reload:  func() error { return nil },
	})
	require.NoError(t, err)
	require.NoError(t, srv.Listen())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})

	// 等待 socket 就绪。
	require.Eventually(t, func() bool {
		_, err := api.NewClient(api.ClientOptions{Listen: "unix://" + sock})
		return err == nil
	}, 2*time.Second, 5*time.Millisecond)
	return sock
}

func TestStatusCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "status", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "node-a")
	assert.Contains(t, out, "10.126.126.1")
	assert.Contains(t, out, "2 (1 direct, 1 relayed)")
}

func TestStatusCommandJSON(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "status", "--socket", sock, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "\"hostname\": \"node-a\"")
}

func TestPeerListCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "peer", "list", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "NODE ID")
	assert.Contains(t, out, "10.126.126.2")
	assert.Contains(t, out, "Direct")
	assert.Contains(t, out, "Relayed")
}

func TestPeerShowCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "peer", "show", "1122334455667788", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "10.126.126.2")
	assert.Contains(t, out, "Direct")
}

func TestPeerShowUnknown(t *testing.T) {
	sock := startMockAPI(t)
	_, err := execute(t, "peer", "show", "ffffffff", "--socket", sock)
	require.Error(t, err)
}

func TestRouteListCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "route", "list", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "DESTINATION")
	assert.Contains(t, out, "10.126.126.3")
	assert.Contains(t, out, "HOPS")
}

func TestTopoCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "topo", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "[self]")
	assert.Contains(t, out, "cost")
}

func TestTopoCommandDOT(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "topo", "--dot", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "digraph mesh {")
	assert.Contains(t, out, "->")
	assert.True(t, strings.Contains(out, "style=filled"), "self 节点应高亮")
}

func TestConfigReloadCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "config", "reload", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "reloaded")
}

func TestAdminConnectFailure(t *testing.T) {
	// 指向一个不存在的 socket,应返回可读的错误。
	_, err := execute(t, "status", "--socket", "unix:///tmp/does-not-exist-mesh.sock")
	require.Error(t, err)
}

func TestConfigValidateCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "validate", "-c", p)
	require.NoError(t, err)
	assert.Contains(t, out, "OK")
}

func TestConfigValidateCommandInvalid(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "bad.toml", "[network]\nname = \"\"\n")

	_, err := execute(t, "config", "validate", "-c", p)
	require.Error(t, err)
}

func TestConfigShowCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "show", "-c", p)
	require.NoError(t, err)
	assert.Contains(t, out, "test-net")
}

func TestConfigShowRedactsSecret(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", `
[network]
name   = "test-net"
secret = "topsecret"

[[listen]]
addr = "udp://127.0.0.1:0"
`)

	out, err := execute(t, "config", "show", "-c", p)
	require.NoError(t, err)
	assert.NotContains(t, out, "topsecret")
	assert.Contains(t, out, "masked")
}

func TestConfigRawCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "raw", p)
	require.NoError(t, err)
	assert.Contains(t, out, "test-net")
}

func TestConfigRawCommandMissingFile(t *testing.T) {
	_, err := execute(t, "config", "raw", "/nonexistent/x.toml")
	require.Error(t, err)
}

func TestConfigExportCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "export", "-c", p)
	require.NoError(t, err)
	assert.Contains(t, out, "test-net")
}

func TestConfigExpandCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "expand", "-c", p)
	require.NoError(t, err)
	assert.Contains(t, out, p)
}

func TestConfigSourcesCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "sources", "-c", p)
	require.NoError(t, err)
	assert.Contains(t, out, "network.name")
}

func TestConfigDiffCommand(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "ok.toml", minimalConfig)

	out, err := execute(t, "config", "diff", "-c", p)
	require.NoError(t, err)
	assert.NotEmpty(t, out)
}

func TestRootHelpListsCommands(t *testing.T) {
	out, err := execute(t, "--help")
	require.NoError(t, err)
	for _, sub := range []string{"status", "peer", "route", "topo", "config", "node", "version"} {
		assert.Contains(t, out, sub, "根帮助应列出 %s", sub)
	}
}

func TestNATCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "nat", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "cone")
	assert.Contains(t, out, "203.0.113.5:45231")
	assert.Contains(t, out, "Candidates:")
}

func TestNATCommandJSON(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "nat", "--socket", sock, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "\"type\": \"cone\"")
}

func TestNATCandidatesCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "nat", "candidates", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "TYPE")
	assert.Contains(t, out, "192.168.1.10:11010")
	assert.Contains(t, out, "srflx")
}

func TestNATProbeCommand(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "nat", "probe", "--socket", sock)
	require.NoError(t, err)
	assert.Contains(t, out, "Probing NAT type...", "probe 应提示正在探测")
	assert.Contains(t, out, "NAT Type")
	assert.Contains(t, out, "203.0.113.5:45231")
}

func TestNATProbeCommandJSON(t *testing.T) {
	sock := startMockAPI(t)
	out, err := execute(t, "nat", "probe", "--socket", sock, "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "Probing", "--json 不应输出提示文本")
	assert.Contains(t, out, "\"type\": \"cone\"")
}
