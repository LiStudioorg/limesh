package device

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenNegativeMTU 验证非法 MTU 被拒绝。
func TestOpenNegativeMTU(t *testing.T) {
	_, err := Open(Config{Name: "mesh0", MTU: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative mtu")
}

// TestOpenEitherSucceedsOrReportsPermission 在无 root 环境验证 Open 返回
// 可识别的权限错误;在有 root 环境则验证能真正打开设备。
//
// 这样测试在两种环境下都有效:CI(无 root)覆盖权限错误路径与提示文案;
// 开发者机器(root)覆盖真实创建路径。
func TestOpenEitherSucceedsOrReportsPermission(t *testing.T) {
	dev, err := Open(Config{Name: "meshtest0", MTU: 1380})
	if err == nil {
		// 有权限:验证能正常关闭。
		t.Cleanup(func() { _ = dev.Close() })
		assert.NotEmpty(t, dev.Name())
		assert.Equal(t, 1380, dev.MTU())
		return
	}

	// 无权限或未支持:必须是可识别的权限/不支持错误,且带修复提示。
	require.True(t,
		IsPermissionError(err) || errors.Is(err, ErrUnsupported),
		"Open 失败应是权限或未支持错误,实际: %v", err)
	if IsPermissionError(err) {
		assert.Contains(t, err.Error(), "setcap", "权限错误应包含 Linux 修复提示")
		assert.Contains(t, err.Error(), "Administrator", "权限错误应包含 Windows 提示")
	}
}

// TestFromFDInvalid 验证非法 fd 被拒绝。
func TestFromFDInvalid(t *testing.T) {
	_, err := FromFD(-1, 1380)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid fd")
}

// TestWrapPermissionError 覆盖权限错误识别与文案。
func TestWrapPermissionError(t *testing.T) {
	// os.ErrPermission 应被识别。
	err := wrapPermissionError(os.ErrPermission)
	require.Error(t, err)
	assert.True(t, IsPermissionError(err))
	assert.Contains(t, err.Error(), "permission denied opening TUN")

	// "operation not permitted" 字符串应被识别(裸 EPERM 场景)。
	err = wrapPermissionError(errors.New("operation not permitted"))
	assert.True(t, IsPermissionError(err))

	err = wrapPermissionError(errors.New("permission denied"))
	assert.True(t, IsPermissionError(err))

	// 非权限错误应原样返回。
	plain := errors.New("device not found")
	err = wrapPermissionError(plain)
	assert.False(t, IsPermissionError(err))
	assert.ErrorIs(t, err, plain)

	// nil 安全。
	assert.NoError(t, wrapPermissionError(nil))
}

// TestIsPermString 覆盖字符串兜底识别。
func TestIsPermString(t *testing.T) {
	assert.True(t, isPermString(errors.New("Operation Not Permitted")))
	assert.True(t, isPermString(errors.New("Permission denied")))
	assert.True(t, isPermString(errors.New("Access is denied.")))
	assert.False(t, isPermString(errors.New("no such device")))
	assert.False(t, isPermString(nil))
}

// TestIsPermissionErrorNil 验证 nil 返回 false。
func TestIsPermissionErrorNil(t *testing.T) {
	assert.False(t, IsPermissionError(nil))
	assert.False(t, IsPermissionError(errors.New("x")))
}

// ---------------------------------------------------------------------------
// FakeDevice
// ---------------------------------------------------------------------------

// TestFakeDeviceReadWrite 验证假设备的读写队列语义。
func TestFakeDeviceReadWrite(t *testing.T) {
	d := NewFakeDevice("fake0", 1400)
	defer d.Close()

	assert.Equal(t, "fake0", d.Name())
	assert.Equal(t, 1400, d.MTU())

	// 注入入站包,Read 取回。
	d.Inbound <- []byte("inbound-pkt")
	buf := make([]byte, 64)
	n, err := d.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "inbound-pkt", string(buf[:n]))

	// Write 的包可从 Outbound 读到。
	_, err = d.Write([]byte("outbound-pkt"))
	require.NoError(t, err)
	assert.Equal(t, 1, d.WriteCount())
	select {
	case got := <-d.Outbound:
		assert.Equal(t, "outbound-pkt", string(got))
	default:
		t.Fatal("Outbound 应有写入的包")
	}
}

// TestFakeDeviceDefaultsMTU 验证 MTU 兜底。
func TestFakeDeviceDefaultsMTU(t *testing.T) {
	d := NewFakeDevice("x", 0)
	assert.Equal(t, 1380, d.MTU())
}

// TestFakeDeviceClose 验证关闭后读写报错且幂等。
func TestFakeDeviceClose(t *testing.T) {
	d := NewFakeDevice("fake0", 1380)
	require.NoError(t, d.Close())
	assert.True(t, d.Closed())
	require.NoError(t, d.Close(), "重复关闭应安全")

	_, err := d.Write([]byte("x"))
	require.Error(t, err)

	// 关闭后 Read 返回 io.EOF。
	_, err = d.Read(make([]byte, 8))
	require.Error(t, err)
	assert.ErrorIs(t, err, io.EOF)
}

// TestFakeDeviceInjectedErrors 验证注入的错误路径。
func TestFakeDeviceInjectedErrors(t *testing.T) {
	d := NewFakeDevice("fake0", 1380)
	defer d.Close()

	readErr := errors.New("read boom")
	d.SetReadErr(readErr)
	_, err := d.Read(make([]byte, 8))
	assert.ErrorIs(t, err, readErr)

	d.SetReadErr(nil)
	d.Inbound <- []byte("ok")
	n, err := d.Read(make([]byte, 8))
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	writeErr := errors.New("write boom")
	d.SetWriteErr(writeErr)
	_, err = d.Write([]byte("x"))
	assert.ErrorIs(t, err, writeErr)
}

// TestFakeDeviceWriteDropsWhenFull 验证 Outbound 满时丢弃而非阻塞。
func TestFakeDeviceWriteDropsWhenFull(t *testing.T) {
	d := NewFakeDevice("fake0", 1380)
	defer d.Close()

	// 队列容量 64,写满后再写不应阻塞。
	for i := 0; i < cap(d.Outbound)+10; i++ {
		_, err := d.Write([]byte{byte(i)})
		require.NoError(t, err)
	}
	assert.Equal(t, cap(d.Outbound)+10, d.WriteCount(), "写入计数包含被丢弃的包")
}

// TestPermissionHintContent 确保提示文案包含各平台关键字。
func TestPermissionHintContent(t *testing.T) {
	for _, kw := range []string{"Linux", "macOS", "Windows", "Docker", "K8s", "setcap"} {
		assert.True(t, strings.Contains(permissionHint, kw), "提示应包含 %q", kw)
	}
}
