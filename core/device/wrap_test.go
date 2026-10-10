package device

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/tun"
)

// fakeTunNative 实现 wireguard-go 的 tun.Device 批量接口,用于覆盖适配层。
type fakeTunNative struct {
	mtu      int
	name     string
	readPkts [][]byte
	written  [][]byte
	closed   bool
	readErr  error
	writeErr error
	nameErr  error
	mtuErr   error
}

func (f *fakeTunNative) File() *os.File { return nil }

func (f *fakeTunNative) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if len(f.readPkts) == 0 {
		return 0, nil
	}
	pkt := f.readPkts[0]
	f.readPkts = f.readPkts[1:]
	n := copy(bufs[0][offset:], pkt)
	sizes[0] = n
	return 1, nil
}

func (f *fakeTunNative) Write(bufs [][]byte, offset int) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	cp := make([]byte, len(bufs[0])-offset)
	copy(cp, bufs[0][offset:])
	f.written = append(f.written, cp)
	return 1, nil
}

func (f *fakeTunNative) MTU() (int, error) {
	if f.mtuErr != nil {
		return 0, f.mtuErr
	}
	return f.mtu, nil
}

func (f *fakeTunNative) Name() (string, error) {
	if f.nameErr != nil {
		return "", f.nameErr
	}
	return f.name, nil
}

func (f *fakeTunNative) Events() <-chan tun.Event { return nil }
func (f *fakeTunNative) Close() error             { f.closed = true; return nil }
func (f *fakeTunNative) BatchSize() int           { return 1 }

// TestNewNativeDeviceNil 验证 nil 设备被拒绝。
func TestNewNativeDeviceNil(t *testing.T) {
	_, err := newNativeDevice(nil, 1380)
	require.Error(t, err)
}

// TestNewNativeDeviceMTUFallback 验证 mtu<=0 时从设备读取。
func TestNewNativeDeviceMTUFallback(t *testing.T) {
	native := &fakeTunNative{mtu: 1280, name: "n0"}
	d, err := newNativeDevice(native, 0)
	require.NoError(t, err)
	assert.Equal(t, 1280, d.MTU())

	// MTU 读取失败时返回错误。
	bad := &fakeTunNative{mtuErr: errors.New("no mtu")}
	_, err = newNativeDevice(bad, 0)
	require.Error(t, err)
}

// TestNativeDeviceReadWrite 覆盖适配层的单包读写。
func TestNativeDeviceReadWrite(t *testing.T) {
	native := &fakeTunNative{
		mtu:      1400,
		name:     "mesh0",
		readPkts: [][]byte{[]byte("pkt-one"), []byte("pkt-two")},
	}
	d, err := newNativeDevice(native, 1400)
	require.NoError(t, err)

	assert.Equal(t, "mesh0", d.Name())
	assert.Equal(t, 1400, d.MTU())

	buf := make([]byte, 64)
	n, err := d.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "pkt-one", string(buf[:n]))

	_, err = d.Write([]byte("out"))
	require.NoError(t, err)
	require.Len(t, native.written, 1)
	assert.Equal(t, "out", string(native.written[0]))

	require.NoError(t, d.Close())
	assert.True(t, native.closed)
}

// TestNativeDeviceReadEmptyBuffer 验证空缓冲被拒绝。
func TestNativeDeviceReadEmptyBuffer(t *testing.T) {
	native := &fakeTunNative{mtu: 1380}
	d, err := newNativeDevice(native, 1380)
	require.NoError(t, err)

	_, err = d.Read(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty buffer")
}

// TestNativeDeviceReadNoPacket 验证无包时返回 0,nil。
func TestNativeDeviceReadNoPacket(t *testing.T) {
	native := &fakeTunNative{mtu: 1380}
	d, err := newNativeDevice(native, 1380)
	require.NoError(t, err)

	n, err := d.Read(make([]byte, 8))
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestNativeDeviceReadError 验证底层读错误透传。
func TestNativeDeviceReadError(t *testing.T) {
	boom := errors.New("read boom")
	native := &fakeTunNative{readErr: boom}
	d, err := newNativeDevice(native, 1380)
	require.NoError(t, err)

	_, err = d.Read(make([]byte, 8))
	assert.ErrorIs(t, err, boom)
}

// TestNativeDeviceNameError 验证名字读取失败返回空串而非 panic。
func TestNativeDeviceNameError(t *testing.T) {
	native := &fakeTunNative{nameErr: errors.New("no name")}
	d, err := newNativeDevice(native, 1380)
	require.NoError(t, err)
	assert.Equal(t, "", d.Name())
}

// TestFromFDPositiveInvalid 验证正数但非法的 fd 在 Linux 上报错。
func TestFromFDPositiveInvalid(t *testing.T) {
	// 用一个必然无效的大 fd。Linux 会返回错误;其它平台返回 ErrUnsupported。
	_, err := FromFD(999999, 1380)
	require.Error(t, err)
}
