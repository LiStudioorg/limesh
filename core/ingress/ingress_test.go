package ingress

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/device"
	"github.com/LiStudioorg/limesh/core/packet"
)

// testConfig 返回一份合法的测试配置。
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Network.Name = "test-net"
	cfg.Network.Hostname = "node-a"
	cfg.Listen = []config.ListenConfig{{Addr: "udp://127.0.0.1:0"}}
	cfg.Peers = []string{}
	require.NoError(t, cfg.Validate())
	return cfg
}

// newTUNWithFake 构造一个绑定假设备的 TUN 接入层。
func newTUNWithFake(t *testing.T, cfg *config.Config) (*TUNIngress, *device.FakeDevice) {
	t.Helper()
	fake := device.NewFakeDevice("mesh-test", 1380)
	ing, err := NewTUN(Options{Config: cfg, Logger: zap.NewNop(), Device: fake})
	require.NoError(t, err)
	return ing, fake
}

// TestNewRejectsNilConfig 验证 nil 配置被拒绝。
func TestNewRejectsNilConfig(t *testing.T) {
	_, err := New(Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config must not be nil")
}

// TestNewTUNMode 验证 tun 模式返回 TUNIngress。
func TestNewTUNMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ingress.Mode = "tun"

	fake := device.NewFakeDevice("mesh0", 1380)
	ing, err := New(Options{Config: cfg, Logger: zap.NewNop(), Device: fake})
	require.NoError(t, err)
	defer ing.Close()

	assert.Equal(t, ModeTUN, ing.Mode())
}

// TestNewNoTUNNotImplemented 验证 no-tun 模式明确返回未实现错误。
func TestNewNoTUNNotImplemented(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ingress.Mode = "no-tun"

	_, err := New(Options{Config: cfg, Logger: zap.NewNop()})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotImplemented)
	assert.Contains(t, err.Error(), "no-tun")
}

// TestNewUnknownMode 验证未知模式被拒绝。
func TestNewUnknownMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ingress.Mode = "bogus"

	_, err := New(Options{Config: cfg, Logger: zap.NewNop()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")
}

// TestNewAutoWithFakeDevice 验证 auto 模式在设备可用时成功。
func TestNewAutoWithFakeDevice(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ingress.Mode = "auto"

	fake := device.NewFakeDevice("mesh0", 1380)
	ing, err := New(Options{Config: cfg, Logger: zap.NewNop(), Device: fake})
	require.NoError(t, err)
	defer ing.Close()
	assert.Equal(t, ModeTUN, ing.Mode())
}

// TestNewAutoRealDeviceFails 验证 auto 模式在真实设备不可用(无 root)时
// 返回错误,而非静默成功。该测试在无 root 的 CI 上覆盖失败路径,
// 在有 root 的机器上则真实成功。
func TestNewAutoRealDeviceFails(t *testing.T) {
	cfg := testConfig(t)
	cfg.Ingress.Mode = "auto"
	cfg.Ingress.Interface = "mesh-auto-test0"

	ing, err := New(Options{Config: cfg, Logger: zap.NewNop()})
	if err == nil {
		// 有 root/权限:真实打开成功。
		t.Cleanup(func() { _ = ing.Close() })
		assert.Equal(t, ModeTUN, ing.Mode())
		return
	}
	require.True(t,
		device.IsPermissionError(err) || errors.Is(err, device.ErrUnsupported),
		"auto 失败应是权限或未支持错误,实际: %v", err)
}

// TestReadLoopProducesPackets 验证 TUN 读到的 IP 包经 Outbound 送出。
func TestReadLoopProducesPackets(t *testing.T) {
	cfg := testConfig(t)
	ing, fake := newTUNWithFake(t, cfg)
	defer ing.Close()

	ipPkt := []byte{0x45, 0x00, 0x00, 0x14, 0xde, 0xad} // 假 IPv4 头 + 载荷
	fake.Inbound <- ipPkt

	select {
	case got := <-ing.Outbound():
		require.NotNil(t, got)
		assert.Equal(t, packet.TypeData, got.Type())
		assert.Equal(t, ipPkt, got.Payload())
	case <-time.After(2 * time.Second):
		t.Fatal("Outbound 未在超时内产出包")
	}
}

// TestDeliverWritesToDevice 验证 Deliver 把 IP 载荷写入 TUN。
func TestDeliverWritesToDevice(t *testing.T) {
	cfg := testConfig(t)
	ing, fake := newTUNWithFake(t, cfg)
	defer ing.Close()

	ipPkt := []byte{0x45, 0x00, 0x00, 0x10, 0x01, 0x02}
	pkt := packet.New(ipPkt)
	pkt.SetType(packet.TypeData)

	require.NoError(t, ing.Deliver(pkt))

	select {
	case got := <-fake.Outbound:
		assert.Equal(t, ipPkt, got)
	case <-time.After(2 * time.Second):
		t.Fatal("设备未收到 Deliver 的包")
	}
}

// TestDeliverEmptyPayload 验证空载荷被忽略且不写入。
func TestDeliverEmptyPayload(t *testing.T) {
	cfg := testConfig(t)
	ing, fake := newTUNWithFake(t, cfg)
	defer ing.Close()

	pkt := packet.New(nil)
	require.NoError(t, ing.Deliver(pkt))
	assert.Equal(t, 0, fake.WriteCount())
}

// TestDeliverNilPacket 验证 nil 包被拒绝。
func TestDeliverNilPacket(t *testing.T) {
	cfg := testConfig(t)
	ing, _ := newTUNWithFake(t, cfg)
	defer ing.Close()

	err := ing.Deliver(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil packet")
}

// TestDeliverAfterClose 验证关闭后 Deliver 报错。
func TestDeliverAfterClose(t *testing.T) {
	cfg := testConfig(t)
	ing, _ := newTUNWithFake(t, cfg)
	require.NoError(t, ing.Close())

	pkt := packet.New([]byte{0x45})
	err := ing.Deliver(pkt)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClosed)
}

// TestCloseClosesDeviceAndLoop 验证 Close 关闭设备并使读循环退出。
func TestCloseClosesDeviceAndLoop(t *testing.T) {
	cfg := testConfig(t)
	ing, fake := newTUNWithFake(t, cfg)

	require.NoError(t, ing.Close())
	assert.True(t, fake.Closed())
	require.NoError(t, ing.Close(), "重复关闭应安全")

	// 读循环退出后 Outbound 通道被关闭。
	select {
	case _, ok := <-ing.Outbound():
		assert.False(t, ok, "读循环退出后 Outbound 应关闭")
	case <-time.After(2 * time.Second):
		t.Fatal("Outbound 未在关闭后关闭")
	}
}

// TestDeliverWriteError 验证设备写错误被包装返回。
func TestDeliverWriteError(t *testing.T) {
	cfg := testConfig(t)
	ing, fake := newTUNWithFake(t, cfg)
	defer ing.Close()

	fake.SetWriteErr(errors.New("disk full"))
	err := ing.Deliver(packet.New([]byte{0x45}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write tun")
}

// TestReadLoopStopsOnDeviceError 验证设备读错误终止读循环。
func TestReadLoopStopsOnDeviceError(t *testing.T) {
	cfg := testConfig(t)
	fake := device.NewFakeDevice("mesh-test", 1380)
	ing, err := NewTUN(Options{Config: cfg, Logger: zap.NewNop(), Device: fake})
	require.NoError(t, err)
	defer ing.Close()

	fake.SetReadErr(errors.New("device gone"))
	// 触发一次读:循环应退出并关闭 Outbound。
	fake.Inbound <- []byte{0x45}

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-ing.Outbound():
			return !ok
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "读循环应在设备错误后退出")
}

// TestDeviceNameAndMTU 验证访问器返回设备信息。
func TestDeviceNameAndMTU(t *testing.T) {
	cfg := testConfig(t)
	fake := device.NewFakeDevice("mesh-named", 1420)
	ing, err := NewTUN(Options{Config: cfg, Logger: zap.NewNop(), Device: fake})
	require.NoError(t, err)
	defer ing.Close()

	assert.Equal(t, "mesh-named", ing.DeviceName())
	assert.Equal(t, 1420, ing.MTU())
}
