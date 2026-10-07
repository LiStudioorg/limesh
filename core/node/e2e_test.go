package node

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/transport"
)

// TestTwoNodesInterconnect 是 Phase 0 的核心验收:两个节点通过真实 UDP
// 建立互联,并成功交换 ZCPacket。
//
// 断言链路:
//
//	节点 A 监听 → 节点 B 拨号 → B 写入 ZCPacket → A 读取并解析 → 字段一致
//
// 这是"能编译、能运行、两个节点能手动互联并互发消息"的最小证明。
func TestTwoNodesInterconnect(t *testing.T) {
	logger := zap.NewNop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ---- 节点 A:监听 ----
	cfgA := config.Default()
	cfgA.Network.Name = "e2e"
	cfgA.Network.Hostname = "node-a"
	cfgA.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfgA.Peers = []string{}
	require.NoError(t, cfgA.Validate())

	nodeA, err := New(Options{Config: cfgA, Logger: logger})
	require.NoError(t, err)
	defer nodeA.Close()

	runErr := make(chan error, 1)
	go func() { runErr <- nodeA.Run(ctx) }()

	require.Eventually(t, func() bool { return len(nodeA.Addrs()) == 1 },
		3*time.Second, 10*time.Millisecond, "节点 A 应在预期时间内开始监听")

	addrA := nodeA.Addrs()[0]
	_, portA, err := net.SplitHostPort(addrA)
	require.NoError(t, err)

	// ---- 节点 B:声明 A 为对端 ----
	cfgB := config.Default()
	cfgB.Network.Name = "e2e"
	cfgB.Network.Hostname = "node-b"
	cfgB.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfgB.Peers = []string{addrA}
	require.NoError(t, cfgB.Validate())

	nodeB, err := New(Options{Config: cfgB, Logger: logger})
	require.NoError(t, err)
	defer nodeB.Close()

	go func() { _ = nodeB.Run(ctx) }()

	require.Eventually(t, func() bool { return len(nodeB.Addrs()) == 1 },
		3*time.Second, 10*time.Millisecond, "节点 B 应在预期时间内开始监听")

	// B 的配置确实记录了对端 A —— 保证 peers 配置链路可用。
	assert.Equal(t, []string{addrA}, nodeB.Config().Peers)
	assert.Equal(t, portA, func() string {
		_, p, _ := net.SplitHostPort(nodeB.Config().Peers[0])
		return p
	}())

	// ---- A 接受入站连接 ----
	accepted := make(chan transport.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := nodeA.Listeners()[0].Accept(ctx)
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	// ---- B 发起连接并发送一个 ZCPacket ----
	trB := nodeB.Transport()
	connB, err := trB.Dial(ctx, addrA)
	require.NoError(t, err)
	defer connB.Close()

	srcID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	dstID := []byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

	payload := []byte("phase-0 interop")
	pkt := packet.New(payload)
	pkt.SetType(packet.TypeData)
	pkt.SetSessionID(0xAABBCCDD)
	pkt.SetSequence(42)
	require.NoError(t, pkt.SetSrcNodeID(srcID))
	require.NoError(t, pkt.SetDstNodeID(dstID))

	wire, err := pkt.Marshal()
	require.NoError(t, err)

	_, err = connB.Write(wire)
	require.NoError(t, err)

	// ---- A 接收并解析 ----
	var connA transport.Conn
	select {
	case connA = <-accepted:
		defer connA.Close()
	case err := <-acceptErr:
		t.Fatalf("节点 A 接受连接失败: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("节点 A 未在超时内接受连接")
	}

	require.NoError(t, connA.SetReadDeadline(time.Now().Add(5*time.Second)))

	buf := make([]byte, 65535)
	n, err := connA.Read(buf)
	require.NoError(t, err, "节点 A 应收到节点 B 发来的数据报")

	got, err := packet.Unmarshal(buf[:n])
	require.NoError(t, err, "收到的数据应能解析为合法 ZCPacket")

	// ---- 逐字段校验:确认传输链路无损 ----
	assert.Equal(t, packet.TypeData, got.Type())
	assert.Equal(t, uint32(0xAABBCCDD), got.SessionID())
	assert.Equal(t, uint32(42), got.Sequence())
	assert.Equal(t, srcID, got.SrcNodeID())
	assert.Equal(t, dstID, got.DstNodeID())
	assert.Equal(t, payload, got.Payload()[:len(payload)])

	// ---- 关闭节点 A,验证 Run 正常退出 ----
	cancel()
	select {
	case err := <-runErr:
		assert.NoError(t, err, "取消上下文后 Run 应返回 nil")
	case <-time.After(3 * time.Second):
		t.Fatal("节点 A 未在取消后退出")
	}
}

// TestTwoNodesExchangeBothDirections 验证双向收发。
//
// 单向往返只能证明"能发",双向才能证明连接是两个方向都通的。
func TestTwoNodesExchangeBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tr := transport.NewUDP(transport.Options{})

	// 服务端监听。
	ln, err := tr.Listen("127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	lnAddr := ln.Addr().String()

	// 客户端拨号并发送首包,使 Accept 能识别对端。
	client, err := tr.Dial(ctx, lnAddr)
	require.NoError(t, err)
	defer client.Close()

	_, err = client.Write([]byte("ping"))
	require.NoError(t, err)

	server, err := ln.Accept(ctx)
	require.NoError(t, err)
	defer server.Close()

	// 消费 Accept 阶段缓存的首包。
	require.NoError(t, server.SetReadDeadline(time.Now().Add(3*time.Second)))
	first := make([]byte, 1500)
	n, err := server.Read(first)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(first[:n]))

	// 服务端 → 客户端。
	_, err = server.Write([]byte("pong"))
	require.NoError(t, err)

	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	second := make([]byte, 1500)
	n, err = client.Read(second)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(second[:n]))

	// 客户端 → 服务端(第二轮)。
	_, err = client.Write([]byte("ping-again"))
	require.NoError(t, err)

	require.NoError(t, server.SetReadDeadline(time.Now().Add(3*time.Second)))
	third := make([]byte, 1500)
	n, err = server.Read(third)
	require.NoError(t, err)
	assert.Equal(t, "ping-again", string(third[:n]))
}
