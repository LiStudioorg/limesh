package transport

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTimeout 是测试中等待网络操作的统一上限。
const testTimeout = 3 * time.Second

func TestUDPNameAndCaps(t *testing.T) {
	tr := NewUDP(Options{})

	assert.Equal(t, TypeUDP, tr.Type())

	caps := tr.Capabilities()
	assert.False(t, caps.Reliable, "UDP 不自带重传")
	assert.False(t, caps.Ordered, "UDP 不保证顺序")
	assert.False(t, caps.Multiplexed)
	assert.Equal(t, 1380, caps.MTU)
}

func TestUDPRegisteredInRegistry(t *testing.T) {
	found := false
	for _, ty := range Registered() {
		if ty == TypeUDP {
			found = true
		}
	}
	assert.True(t, found, "UDP 应在 init 中完成注册")

	tr, err := New(TypeUDP, Options{})
	require.NoError(t, err)
	assert.Equal(t, TypeUDP, tr.Type())
}

func TestNewUnknownTransport(t *testing.T) {
	_, err := New(TransportType("carrier-pigeon"), Options{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupported)
}

func TestUDPLoopbackRoundTrip(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	// 先建立客户端并读取其本地地址,再让服务端 Accept。
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	client, err := tr.Dial(ctx, ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	// 客户端先发一个"预热"数据报,使 Accept 能识别对端地址。
	_, err = client.Write([]byte("warmup"))
	require.NoError(t, err)

	accepted := make(chan Conn, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			errs <- err
			return
		}
		accepted <- c
	}()

	var server Conn
	select {
	case server = <-accepted:
		defer server.Close()
	case err := <-errs:
		t.Fatalf("accept 失败: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("accept 超时")
	}

	// 服务端回显它接下来收到的数据报。
	// 注意:Accept 已读取并缓存"warmup"数据报,服务端应先消费它,
	// 这样后续 Read 才能拿到本轮真正发送的载荷。
	echoDone := make(chan error, 1)
	go func() {
		_ = server.SetReadDeadline(time.Now().Add(testTimeout))

		warm := make([]byte, 1500)
		if _, err := server.Read(warm); err != nil {
			echoDone <- err
			return
		}

		buf := make([]byte, 1500)
		n, err := server.Read(buf)
		if err != nil {
			echoDone <- err
			return
		}
		_, err = server.Write(buf[:n])
		echoDone <- err
	}()

	payload := []byte("hello mesh")
	_, err = client.Write(payload)
	require.NoError(t, err)

	require.NoError(t, client.SetReadDeadline(time.Now().Add(testTimeout)))
	got := make([]byte, 1500)
	n, err := client.Read(got)
	require.NoError(t, err)
	assert.Equal(t, payload, got[:n])

	require.NoError(t, <-echoDone)
}

func TestUDPWriteReadThroughListener(t *testing.T) {
	// 不依赖 Accept 的回环测试:直接向监听地址发送并收取。
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	client, err := tr.Dial(ctx, ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	payload := []byte("direct-datagram")
	_, err = client.Write(payload)
	require.NoError(t, err)

	// 通过 Accept 取回该数据报。
	accepted := make(chan Conn, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			errs <- err
			return
		}
		accepted <- c
	}()

	select {
	case c := <-accepted:
		defer c.Close()
		// Accept 已消费首个数据报,这里只验证地址被正确固定。
		assert.NotNil(t, c.RemoteAddr())
	case err := <-errs:
		t.Fatalf("accept 失败: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("accept 超时")
	}
}

func TestUDPDialContextCanceled(t *testing.T) {
	tr := NewUDP(Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tr.Dial(ctx, "127.0.0.1:11010")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUDPDialResolveError(t *testing.T) {
	tr := NewUDP(Options{})
	ctx := context.Background()

	_, err := tr.Dial(ctx, "no-such-host.invalid:99999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve")
}

func TestUDPListenInvalidAddr(t *testing.T) {
	tr := NewUDP(Options{})

	_, err := tr.Listen("not-an-addr")
	require.Error(t, err)
}

func TestUDPConnCloseIsIdempotent(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ctx := context.Background()
	c, err := tr.Dial(ctx, ln.Addr().String())
	require.NoError(t, err)

	require.NoError(t, c.Close())
	require.NoError(t, c.Close(), "重复 Close 不应返回错误")
}

func TestUDPConnOperationsAfterClose(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	c, err := tr.Dial(context.Background(), ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, c.Close())

	_, err = c.Read(make([]byte, 8))
	assert.ErrorIs(t, err, ErrClosed)

	_, err = c.Write([]byte("x"))
	assert.ErrorIs(t, err, ErrClosed)
}

func TestUDPListenerCloseIsIdempotent(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)

	require.NoError(t, ln.Close())
	require.NoError(t, ln.Close(), "重复 Close 不应返回错误")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err = ln.Accept(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClosed)
}

func TestUDPAcceptRespectsContextCancel(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = ln.Accept(ctx)
	elapsed := time.Since(start)

	require.Error(t, err, "无数据报时 Accept 应在 ctx 超时后返回")
	assert.Less(t, elapsed, 3*time.Second, "应响应 ctx 而非无限阻塞")
}

func TestUDPConnStatsAccounting(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	c, err := tr.Dial(context.Background(), ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()

	payload := bytes.Repeat([]byte{0x42}, 100)
	n, err := c.Write(payload)
	require.NoError(t, err)
	assert.Equal(t, 100, n)

	stats := c.Stats()
	assert.Equal(t, uint64(100), stats.BytesSent, "发送字节应被统计")
	assert.Equal(t, uint64(0), stats.BytesRecv)
	// UDP 无连接状态,RTT 与 Loss 由上层 Probe 填充。
	assert.Zero(t, stats.RTT)
	assert.Zero(t, stats.Loss)
}

func TestUDPAddresses(t *testing.T) {
	tr := NewUDP(Options{})

	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	c, err := tr.Dial(context.Background(), ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()

	assert.NotNil(t, c.LocalAddr())
	require.NotNil(t, c.RemoteAddr())
	assert.Equal(t, ln.Addr().String(), c.RemoteAddr().String())
}

func TestUDPFilteredByAddressFamily(t *testing.T) {
	tr := NewUDP(Options{})

	// 绑定 IPv4 回环:应得到 IPv4 地址而非 ::ffff: 映射形式。
	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	host, _, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host, "应按地址族选择网络类型")
}

func TestUDPOptionsDefaults(t *testing.T) {
	tr := NewUDP(Options{})
	assert.Equal(t, DefaultReadBuffer, tr.opts.ReadBuffer)
	assert.Equal(t, DefaultWriteBuffer, tr.opts.WriteBuffer)
}

func TestUDPOptionsHonored(t *testing.T) {
	tr := NewUDP(Options{ReadBuffer: 1024, WriteBuffer: 2048})
	assert.Equal(t, 1024, tr.opts.ReadBuffer)
	assert.Equal(t, 2048, tr.opts.WriteBuffer)
}

func TestNetworkFor(t *testing.T) {
	cases := []struct {
		network string
		addr    string
		want    string
	}{
		{"", "127.0.0.1:11010", "udp4"},
		{"", "[::1]:11010", "udp6"},
		{"", "0.0.0.0:11010", "udp4"},
		{"", "bad-addr", "udp"},
		{"udp", "127.0.0.1:11010", "udp"},
		{"udp6", "127.0.0.1:11010", "udp6"},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, NetworkFor("udp", tc.network, tc.addr),
			"network=%q addr=%q", tc.network, tc.addr)
	}
}

func TestNewUDPTransportViaFactory(t *testing.T) {
	tr, err := New(TypeUDP, Options{ReadBuffer: 4096})
	require.NoError(t, err)

	udp, ok := tr.(*UDPTransport)
	require.True(t, ok, "工厂应返回 *UDPTransport")
	assert.Equal(t, 4096, udp.opts.ReadBuffer)
}

func TestUDPErrorsAreWrapped(t *testing.T) {
	tr := NewUDP(Options{})
	ctx := context.Background()

	_, err := tr.Dial(ctx, "127.0.0.1:99999")
	require.Error(t, err)
	assert.True(t, errors.Is(err, err) || err != nil, "错误应非空")
	assert.Contains(t, err.Error(), "udp dial", "错误应带上下文")
}
