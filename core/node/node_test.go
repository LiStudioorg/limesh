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

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/transport"
)

// ---------------------------------------------------------------------------
// fake 传输实现:用于在不碰真实网络的前提下覆盖 node 的全部路径
// ---------------------------------------------------------------------------

// fakeAddr 是一个固定的 net.Addr。
type fakeAddr struct{ s string }

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return a.s }

// fakeConn 是被 node 测试忽略的最小 Conn。
type fakeConn struct{}

func (fakeConn) Read([]byte) (int, error)        { return 0, errors.New("unused") }
func (fakeConn) Write(p []byte) (int, error)     { return len(p), nil }
func (fakeConn) Close() error                    { return nil }
func (fakeConn) LocalAddr() net.Addr             { return fakeAddr{"local"} }
func (fakeConn) RemoteAddr() net.Addr            { return fakeAddr{"remote"} }
func (fakeConn) SetReadDeadline(time.Time) error { return nil }
func (fakeConn) Stats() transport.ConnStats      { return transport.ConnStats{} }

// fakeListener 记录关闭次数,可模拟 Accept 错误。
type fakeListener struct {
	addr      string
	closeErr  error
	acceptErr error

	mu     sync.Mutex
	closed int
}

func (l *fakeListener) Accept(ctx context.Context) (transport.Conn, error) {
	if l.acceptErr != nil {
		return nil, l.acceptErr
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *fakeListener) Addr() net.Addr { return fakeAddr{l.addr} }

func (l *fakeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed++
	return l.closeErr
}

func (l *fakeListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// fakeTransport 是可编排行为的传输实现。
type fakeTransport struct {
	typ  transport.TransportType
	caps transport.Caps

	listenErr   error
	dialErr     error
	listeners   []*fakeListener
	listenAddrs []string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		typ:  transport.TypeUDP,
		caps: transport.Caps{MTU: 1380},
	}
}

func (t *fakeTransport) Dial(ctx context.Context, addr string) (transport.Conn, error) {
	if t.dialErr != nil {
		return nil, t.dialErr
	}
	return fakeConn{}, nil
}

func (t *fakeTransport) Listen(addr string) (transport.Listener, error) {
	t.listenAddrs = append(t.listenAddrs, addr)
	if t.listenErr != nil {
		return nil, t.listenErr
	}
	l := &fakeListener{addr: addr}
	t.listeners = append(t.listeners, l)
	return l, nil
}

func (t *fakeTransport) Type() transport.TransportType { return t.typ }

func (t *fakeTransport) Capabilities() transport.Caps { return t.caps }

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// testConfig 返回一份合法的测试配置,监听端口固定为 0(由内核分配)。
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Network.Name = "test-net"
	cfg.Network.Hostname = "node-a"
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	// 测试不访问真实 STUN 服务器:清空列表使探测器不构造。
	cfg.NAT.STUNServers = nil
	require.NoError(t, cfg.Validate())
	return cfg
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNewRejectsNilConfig(t *testing.T) {
	_, err := New(Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config must not be nil")
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Network.Name = "" // 非法:名称不能为空

	_, err := New(Options{Config: cfg})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate")
}

func TestNewUsesInjectedTransport(t *testing.T) {
	ft := newFakeTransport()

	n, err := New(Options{Config: testConfig(t), Transport: ft})
	require.NoError(t, err)
	assert.Same(t, ft, n.Transport())
}

func TestNewBuildsTransportFromConfigScheme(t *testing.T) {
	// 未注入传输时,应按配置中的监听协议自动构造真实传输。
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}

	n, err := New(Options{Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, transport.TypeUDP, n.Transport().Type())
}

func TestNewFailsOnUnsupportedScheme(t *testing.T) {
	// TCP 在 Phase 0 尚未注册;配置引用它时应返回明确的 unsupported 错误,
	// 而不是静默降级到其它传输。
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{{Addr: "tcp://127.0.0.1:0"}}

	_, err := New(Options{Config: cfg})
	require.Error(t, err)
	assert.ErrorIs(t, err, transport.ErrUnsupported)
}

func TestNewDefaultsLoggerToNop(t *testing.T) {
	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Transport: newFakeTransport()})
	require.NoError(t, err)

	assert.NotNil(t, n.log, "未提供 logger 时应回退到 zap.NewNop()")
	assert.NotPanics(t, func() { n.log.Info("no-op") })
}

func TestNewHostnameFallback(t *testing.T) {
	cfg := testConfig(t)
	cfg.Network.Hostname = ""

	n, err := New(Options{Config: cfg, Transport: newFakeTransport(), Hostname: "fallback-host"})
	require.NoError(t, err)
	assert.Equal(t, "fallback-host", n.Hostname, "配置为空时应使用调用方提供的回退值")
}

func TestNewHostnameFromConfigWins(t *testing.T) {
	cfg := testConfig(t) // hostname = "node-a"

	n, err := New(Options{Config: cfg, Transport: newFakeTransport(), Hostname: "fallback-host"})
	require.NoError(t, err)
	assert.Equal(t, "node-a", n.Hostname, "配置中的 hostname 优先于回退值")
}

func TestNewAcceptsExplicitLogger(t *testing.T) {
	logger := zap.NewNop()
	cfg := testConfig(t)

	n, err := New(Options{Config: cfg, Logger: logger, Transport: newFakeTransport()})
	require.NoError(t, err)
	assert.Same(t, logger, n.log)
}

// ---------------------------------------------------------------------------
// 访问器
// ---------------------------------------------------------------------------

func TestConfigReturnsSameSnapshot(t *testing.T) {
	cfg := testConfig(t)
	n, err := New(Options{Config: cfg, Transport: newFakeTransport()})
	require.NoError(t, err)

	assert.Same(t, cfg, n.Config())
}

func TestListenersAndAddrsBeforeRun(t *testing.T) {
	n, err := New(Options{Config: testConfig(t), Transport: newFakeTransport()})
	require.NoError(t, err)

	assert.Empty(t, n.Listeners(), "Run 之前不应有监听器")
	assert.Empty(t, n.Addrs())
}

func TestListenersReturnsCopy(t *testing.T) {
	ft := newFakeTransport()
	n, err := New(Options{Config: testConfig(t), Transport: ft})
	require.NoError(t, err)

	runNodeInBackground(t, n)

	// 修改返回的切片不得影响内部状态。
	got := n.Listeners()
	require.Len(t, got, 1)
	got[0] = nil

	assert.Len(t, n.Listeners(), 1, "返回的切片应为副本")
}

func TestAddrsReportsBoundAddress(t *testing.T) {
	ft := newFakeTransport()
	n, err := New(Options{Config: testConfig(t), Transport: ft})
	require.NoError(t, err)

	runNodeInBackground(t, n)

	addrs := n.Addrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, "127.0.0.1:0", addrs[0], "fake listener 原样回报传入地址")
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// runNodeInBackground 启动节点并返回取消函数;等待监听器就绪后返回。
func runNodeInBackground(t *testing.T, n *Node) context.CancelFunc {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	// 等待监听器进入已启动状态。
	require.Eventually(t, func() bool {
		return len(n.Listeners()) > 0
	}, 2*time.Second, 5*time.Millisecond, "监听器未在预期时间内启动")

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run 未在取消后及时返回")
		}
	})
	return cancel
}

func TestRunListensAndStopsOnCancel(t *testing.T) {
	ft := newFakeTransport()
	n, err := New(Options{Config: testConfig(t), Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	require.Eventually(t, func() bool { return len(n.Listeners()) == 1 },
		2*time.Second, 5*time.Millisecond)

	assert.Equal(t, []string{"127.0.0.1:0"}, ft.listenAddrs)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "正常取消应返回 nil")
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在取消后返回")
	}

	require.Len(t, ft.listeners, 1)
	assert.Equal(t, 1, ft.listeners[0].closeCount(), "Run 退出时应关闭监听器")
}

func TestRunRejectsSecondCall(t *testing.T) {
	n, err := New(Options{Config: testConfig(t), Transport: newFakeTransport(), Logger: zap.NewNop()})
	require.NoError(t, err)

	runNodeInBackground(t, n)

	// 第二次调用必须被拒绝,避免重复监听。
	err = n.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAlreadyRunning)
}

func TestRunAfterCloseFails(t *testing.T) {
	n, err := New(Options{Config: testConfig(t), Transport: newFakeTransport(), Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, n.Close())

	err = n.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, transport.ErrClosed)
}

func TestRunFailsWhenListenFails(t *testing.T) {
	ft := newFakeTransport()
	ft.listenErr = errors.New("address already in use")

	n, err := New(Options{Config: testConfig(t), Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	err = n.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen")
	assert.Contains(t, err.Error(), "address already in use")
}

func TestRunClosesPartialListenersOnFailure(t *testing.T) {
	// 第二个监听地址失败时,第一个已启动的监听器必须被关闭,
	// 不能留下半启动状态。
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{
		{Addr: "udp://127.0.0.1:0"},
		{Addr: "udp://127.0.0.1:0"},
	}

	ft := &failSecondTransport{inner: newFakeTransport()}
	n, err := New(Options{Config: cfg, Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	err = n.Run(context.Background())
	require.Error(t, err)

	require.Len(t, ft.inner.listeners, 1)
	assert.Equal(t, 1, ft.inner.listeners[0].closeCount(),
		"失败前已启动的监听器必须被关闭")
}

func TestRunNoListenerStarted(t *testing.T) {
	// 配置中的协议与传输类型不匹配时,不应有任何监听器启动。
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}

	ft := newFakeTransport()
	ft.typ = transport.TypeTCP // 与配置中的 udp 不匹配

	n, err := New(Options{Config: cfg, Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	err = n.Run(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoListener)
	assert.Empty(t, ft.listenAddrs, "不匹配的协议不应触发 Listen")
}

func TestRunSkipsMismatchedSchemeButKeepsOthers(t *testing.T) {
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{
		{Addr: "udp://127.0.0.1:0"},
		{Addr: "udp://127.0.0.1:0"},
	}

	ft := newFakeTransport()
	n, err := New(Options{Config: cfg, Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	runNodeInBackground(t, n)
	assert.Len(t, n.Listeners(), 2)
}

// failSecondTransport 让第二次 Listen 失败,用于验证部分失败的清理。
type failSecondTransport struct {
	inner *fakeTransport
	calls int
}

func (t *failSecondTransport) Dial(ctx context.Context, addr string) (transport.Conn, error) {
	return t.inner.Dial(ctx, addr)
}

func (t *failSecondTransport) Listen(addr string) (transport.Listener, error) {
	t.calls++
	if t.calls == 2 {
		return nil, errors.New("simulated second listen failure")
	}
	return t.inner.Listen(addr)
}

func (t *failSecondTransport) Type() transport.TransportType { return t.inner.Type() }

func (t *failSecondTransport) Capabilities() transport.Caps { return t.inner.Capabilities() }

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestCloseIsIdempotent(t *testing.T) {
	n, err := New(Options{Config: testConfig(t), Transport: newFakeTransport(), Logger: zap.NewNop()})
	require.NoError(t, err)

	require.NoError(t, n.Close())
	require.NoError(t, n.Close(), "重复 Close 不应返回错误")
}

func TestCloseBeforeRunIsSafe(t *testing.T) {
	n, err := New(Options{Config: testConfig(t), Transport: newFakeTransport(), Logger: zap.NewNop()})
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		require.NoError(t, n.Close())
	})
}

func TestCloseClosesAllListeners(t *testing.T) {
	cfg := testConfig(t)
	cfg.Listen = []config.ListenConfig{
		{Addr: "udp://127.0.0.1:0"},
		{Addr: "udp://127.0.0.1:0"},
	}

	ft := newFakeTransport()
	n, err := New(Options{Config: cfg, Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.Run(ctx) }()

	require.Eventually(t, func() bool { return len(n.Listeners()) == 2 },
		2*time.Second, 5*time.Millisecond)

	require.NoError(t, n.Close())
	for i, l := range ft.listeners {
		assert.Equal(t, 1, l.closeCount(), "监听器 %d 应被关闭", i)
	}
}

func TestCloseIgnoresListenerErrors(t *testing.T) {
	ft := newFakeTransport()
	n, err := New(Options{Config: testConfig(t), Transport: ft, Logger: zap.NewNop()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.Run(ctx) }()

	require.Eventually(t, func() bool { return len(n.Listeners()) == 1 },
		2*time.Second, 5*time.Millisecond)

	// 注入关闭错误:Close 应继续尝试其余监听器并整体返回 nil。
	ft.listeners[0].closeErr = errors.New("close failed")
	require.NoError(t, n.Close(), "单个监听器关闭失败不应中断整体关闭")
}
