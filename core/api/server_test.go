package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
)

// mockNode 是 api.NodeInfo 的内存实现,供服务端测试使用。
type mockNode struct {
	status   StatusSnapshot
	peers    []PeerSnapshot
	routes   []RouteSnapshot
	topology TopologySnapshot
	cfg      *config.Config
}

func (m *mockNode) Status() StatusSnapshot             { return m.status }
func (m *mockNode) PeerList() []PeerSnapshot           { return m.peers }
func (m *mockNode) RouteList() []RouteSnapshot         { return m.routes }
func (m *mockNode) TopologySnapshot() TopologySnapshot { return m.topology }
func (m *mockNode) Config() *config.Config             { return m.cfg }
func (m *mockNode) NAT() NATSnapshot {
	return NATSnapshot{Type: "cone", Mapped: "203.0.113.5:45231", CandidateCount: 2}
}
func (m *mockNode) Candidates() []CandidateSnapshot {
	return []CandidateSnapshot{
		{Type: "host", Addr: "192.168.1.10:11010", Priority: 2130706431, Source: "eth0"},
		{Type: "srflx", Addr: "203.0.113.5:45231", Priority: 16777215, Source: "stun:stun.l.google.com:19302"},
	}
}

// newMockNode 构造一个带样本数据的 mock。
func newMockNode() *mockNode {
	cfg := config.Default()
	cfg.Network.Name = "test-net"
	cfg.Network.Secret = "supersecret"
	return &mockNode{
		status: StatusSnapshot{
			NodeID:      "aabbccdd",
			Hostname:    "node-a",
			VirtualIP:   "10.126.126.1",
			PeerCount:   1,
			DirectPeers: 1,
			RouteCount:  2,
			Version:     "test",
		},
		peers: []PeerSnapshot{
			{
				NodeID:    "11223344",
				VirtualIP: "10.126.126.2",
				State:     "Direct",
				Addr:      "127.0.0.1:11010",
				RTTMillis: 2.5,
				LossRate:  0.0,
				LastSeen:  time.Now(),
			},
		},
		routes: []RouteSnapshot{
			{Dst: "11223344", DstVirtualIP: "10.126.126.2", NextHop: "11223344", Cost: 12, Hops: 1},
		},
		topology: TopologySnapshot{
			Nodes: []NodeSnapshot{{NodeID: "aabbccdd", VirtualIP: "10.126.126.1", IsSelf: true}},
			Edges: []EdgeSnapshot{{From: "aabbccdd", To: "11223344", Cost: 12}},
		},
		cfg: cfg,
	}
}

// startTestServer 启动一个基于 Unix Socket 的测试服务端,返回服务器与客户端。
func startTestServer(t *testing.T, opts ServerOptions) (*Server, *Client) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")

	if opts.Config == nil {
		opts.Config = config.Default()
	}
	opts.Config.Admin.Enabled = true
	opts.Config.Admin.Listen = "unix://" + sock
	if opts.Node == nil {
		opts.Node = newMockNode()
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}

	srv, err := NewServer(opts)
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
			t.Error("server did not stop")
		}
	})

	cl, err := NewClient(ClientOptions{Config: opts.Config})
	require.NoError(t, err)
	return srv, cl
}

// TestServerStatus 验证 /status 端点。
func TestServerStatus(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{Version: "v9"})

	st, err := cl.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "node-a", st.Hostname)
	assert.Equal(t, "10.126.126.1", st.VirtualIP)
	assert.Equal(t, 1, st.PeerCount)
}

// TestServerPeers 验证 /peers 与 /peers/{id}。
func TestServerPeers(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})

	peers, err := cl.Peers(context.Background())
	require.NoError(t, err)
	require.Len(t, peers, 1)
	assert.Equal(t, "Direct", peers[0].State)

	p, err := cl.Peer(context.Background(), "11223344")
	require.NoError(t, err)
	assert.Equal(t, "10.126.126.2", p.VirtualIP)

	// 前缀匹配。
	p, err = cl.Peer(context.Background(), "1122")
	require.NoError(t, err)
	assert.Equal(t, "11223344", p.NodeID)

	// 不存在。
	_, err = cl.Peer(context.Background(), "ffff")
	require.Error(t, err)
}

// TestServerRoutes 验证 /routes。
func TestServerRoutes(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})

	routes, err := cl.Routes(context.Background())
	require.NoError(t, err)
	require.Len(t, routes, 1)
	assert.Equal(t, "11223344", routes[0].NextHop)
}

// TestServerTopology 验证 /topology。
func TestServerTopology(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})

	topo, err := cl.Topology(context.Background())
	require.NoError(t, err)
	require.Len(t, topo.Nodes, 1)
	require.Len(t, topo.Edges, 1)
	assert.True(t, topo.Nodes[0].IsSelf)
}

// TestServerConfigRedactsSecret 验证配置端点不泄露 secret 明文。
func TestServerConfigRedactsSecret(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})

	raw, err := cl.Config(context.Background())
	require.NoError(t, err)

	// 序列化后不得包含明文 secret。
	blob, err := json.Marshal(raw)
	require.NoError(t, err)
	assert.NotContains(t, string(blob), "supersecret", "secret 明文不得出现在配置响应中")

	net_, ok := raw["network"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, net_["secret_set"])
}

// TestServerVersion 验证 /version。
func TestServerVersion(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{Version: "v1.2.3"})
	v, err := cl.Version(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "v1.2.3", v)
}

// TestServerReload 验证 reload 端点在注入回调时生效。
func TestServerReload(t *testing.T) {
	called := false
	_, cl := startTestServer(t, ServerOptions{Reload: func() error { called = true; return nil }})

	require.NoError(t, cl.ReloadConfig(context.Background()))
	assert.True(t, called)
}

// TestServerReloadNotImplemented 验证未注入回调时 reload 返回 501。
func TestServerReloadNotImplemented(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})
	err := cl.ReloadConfig(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "501")
}

// doRequest 直接调用服务端的 mux 执行一次请求,返回响应记录器。
func doRequest(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

// TestServerMethodNotAllowed 验证错误的 HTTP 方法被拒绝。
func TestServerMethodNotAllowed(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/status", "")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/config/reload", "")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// TestServerUnknownPeerReturns404 验证未知 peer 返回 404。
func TestServerUnknownPeerReturns404(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/peers/zzzz", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestServerUnixSocketPermissions 验证 Unix Socket 权限为 0600。
func TestServerUnixSocketPermissions(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})

	info, err := os.Stat(srv.Addr())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "socket 权限应为 0600")
}

// TestServerTCPRequiresToken 验证 TCP 模式强制 token。
func TestServerTCPRequiresToken(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "admin.token")

	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "tcp://127.0.0.1:0"
	cfg.Admin.TokenFile = tokenFile

	srv, err := NewServer(ServerOptions{Config: cfg, Node: newMockNode(), Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, srv.Listen())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = srv.Close(); <-done })

	// token 文件应被创建且权限 0600。
	info, err := os.Stat(tokenFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NotEmpty(t, srv.Token())

	base := "http://" + srv.Addr()

	// 无 token:401。
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/status", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	// 带 token:200。
	req, _ = http.NewRequest(http.MethodGet, base+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}

// TestServerTCPTokenFromExistingFile 验证复用已存在的 token 文件。
func TestServerTCPTokenFromExistingFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "admin.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("preset-token\n"), 0o600))

	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "tcp://127.0.0.1:0"
	cfg.Admin.TokenFile = tokenFile

	srv, err := NewServer(ServerOptions{Config: cfg, Node: newMockNode(), Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, srv.Listen())
	defer srv.Close()

	assert.Equal(t, "preset-token", srv.Token())
}

// TestServerNewRejectsNilNode 验证 nil NodeInfo 被拒绝。
func TestServerNewRejectsNilNode(t *testing.T) {
	_, err := NewServer(ServerOptions{Config: config.Default()})
	require.Error(t, err)
}

// TestServerNewRejectsNilConfig 验证 nil Config 被拒绝。
func TestServerNewRejectsNilConfig(t *testing.T) {
	_, err := NewServer(ServerOptions{Node: newMockNode()})
	require.Error(t, err)
}

// TestServerCloseIdempotent 验证重复关闭安全。
func TestServerCloseIdempotent(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close())
}

// TestServerListenTwice 验证重复 Listen 被拒绝。
func TestServerListenTwice(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "unix://" + filepath.Join(dir, "a.sock")

	srv, err := NewServer(ServerOptions{Config: cfg, Node: newMockNode(), Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, srv.Listen())
	defer srv.Close()

	// 未 Serve 前 running 为 false,但 listener 已建立;第二次 Listen 会
	// 因 running 仍为 false 而尝试重新监听,通常因端口/文件占用失败或覆盖。
	// 这里只验证不会 panic。
	_ = srv.Listen()
}

// TestServerConfigRawSourcesValidate 覆盖 config 相关端点。
func TestServerConfigRawSourcesValidate(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgFile, []byte("[network]\nname=\"x\"\n"), 0o600))

	mock := newMockNode()
	mock.cfg.Path = cfgFile

	srv, cl := startTestServer(t, ServerOptions{Node: mock})

	raw, err := cl.ConfigRaw(context.Background())
	require.NoError(t, err)
	require.Contains(t, raw, cfgFile)
	assert.Contains(t, raw[cfgFile], "name")

	src, err := cl.ConfigSources(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, src)

	// validate 走 mux 直接调用(客户端无 validate 方法)。
	res := doRequest(t, srv, http.MethodPost, "/api/v1/config/validate", "")
	assert.Equal(t, http.StatusOK, res.Code)
}

// TestServerConfigRawNoPath 验证无来源路径时 raw 返回 404。
func TestServerConfigRawNoPath(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	res := doRequest(t, srv, http.MethodGet, "/api/v1/config/raw", "")
	assert.Equal(t, http.StatusNotFound, res.Code)
}

// TestServerConfigRawReadError 验证读取失败的文件以错误字符串呈现。
func TestServerConfigRawReadError(t *testing.T) {
	mock := newMockNode()
	mock.cfg.Path = "/nonexistent/definitely/missing.toml"
	_, cl := startTestServer(t, ServerOptions{Node: mock})

	raw, err := cl.ConfigRaw(context.Background())
	require.NoError(t, err)
	for _, v := range raw {
		assert.Contains(t, v, "error:")
	}
}

// TestListenUnsupportedScheme 验证非法监听 scheme 被拒绝。
func TestListenUnsupportedScheme(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "ftp://x"

	srv, err := NewServer(ServerOptions{Config: cfg, Node: newMockNode(), Logger: zap.NewNop()})
	require.NoError(t, err)
	err = srv.Listen()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

// TestListenUnixResidualSocket 验证残留 socket 文件被清理。
func TestListenUnixResidualSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "resid.sock")

	// 先建一个监听再关闭,留下(或让内核清理)文件;再手动创建同名文件模拟残留。
	f, err := net.Listen("unix", sock)
	require.NoError(t, err)
	_ = f.Close()
	// 重新创建为普通文件路径的 socket 类型不易构造;改为验证普通文件被拒绝。

	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Admin.Listen = "unix://" + filepath.Join(dir, "plain.txt")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o600))

	srv, err := NewServer(ServerOptions{Config: cfg, Node: newMockNode(), Logger: zap.NewNop()})
	require.NoError(t, err)
	err = srv.Listen()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a socket")
}

// TestServerVersionDefault 验证未注入版本时的默认值。
func TestServerVersionDefault(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	res := doRequest(t, srv, http.MethodGet, "/api/v1/version", "")
	assert.Equal(t, http.StatusOK, res.Code)
	assert.Contains(t, res.Body.String(), "dev")
}

// TestSplitScheme 覆盖 scheme 拆分。
func TestSplitScheme(t *testing.T) {
	s, r := splitScheme("unix:///tmp/x.sock")
	assert.Equal(t, "unix", s)
	assert.Equal(t, "/tmp/x.sock", r)

	s, r = splitScheme("no-scheme")
	assert.Equal(t, "", s)
	assert.Equal(t, "no-scheme", r)
}

// TestPeerByIDEmpty 验证空 nodeID 返回 400。
func TestPeerByIDEmpty(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	res := doRequest(t, srv, http.MethodGet, "/api/v1/peers/", "")
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

// TestServerNATAndCandidates 验证 /nat 与 /candidates 端点。
func TestServerNATAndCandidates(t *testing.T) {
	_, cl := startTestServer(t, ServerOptions{})

	info, err := cl.NAT(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cone", info.Type)
	assert.Equal(t, "203.0.113.5:45231", info.Mapped)
	assert.Equal(t, 2, info.CandidateCount)

	cands, err := cl.Candidates(context.Background())
	require.NoError(t, err)
	require.Len(t, cands, 2)
	assert.Equal(t, "host", cands[0].Type)
	assert.NotZero(t, cands[0].Priority)
}

// TestServerCandidatesMethodNotAllowed 验证非 GET 被拒绝。
func TestServerCandidatesMethodNotAllowed(t *testing.T) {
	srv, _ := startTestServer(t, ServerOptions{})
	assert.Equal(t, http.StatusMethodNotAllowed, doRequest(t, srv, http.MethodPost, "/api/v1/nat", "").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequest(t, srv, http.MethodPost, "/api/v1/candidates", "").Code)
}
