package device

import (
	"errors"
	"strings"

	"golang.zx2c4.com/wireguard/tun"
)

// nativeDevice 把 wireguard-go/tun 的批量 Device 适配为单包 Device。
//
// wireguard-go 的接口以"一批包"为单位读写以摊薄系统调用开销。本项目的
// Ingress 一次处理一个 IP 包,因此在适配层把批次固定为 1,语义更直观,
// 也避免了上层误用批处理导致的顺序问题。
type nativeDevice struct {
	dev tun.Device
	mtu int
	// bufs/sizes 复用与批量接口交互的切片,避免每次 Read 分配。
	bufs  [][]byte
	sizes []int
}

// newNativeDevice 包装一个 tun.Device。
func newNativeDevice(dev tun.Device, mtu int) (*nativeDevice, error) {
	if dev == nil {
		return nil, errors.New("device: nil tun device")
	}
	if mtu <= 0 {
		m, err := dev.MTU()
		if err != nil {
			return nil, err
		}
		mtu = m
	}
	return &nativeDevice{
		dev:   dev,
		mtu:   mtu,
		bufs:  make([][]byte, 1),
		sizes: make([]int, 1),
	}, nil
}

// Read 读取一个 IP 包。
//
// 传入 buf 作为落地缓冲;wireguard-go 支持以 offset 读写,这里 offset 传 0。
func (d *nativeDevice) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, errors.New("device: read: empty buffer")
	}
	d.bufs[0] = buf
	d.sizes[0] = 0

	n, err := d.dev.Read(d.bufs, d.sizes, 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		// 无包可用(理论上 Read 会阻塞,这里兜底避免上层误判为 0 长包)。
		return 0, nil
	}
	return d.sizes[0], nil
}

// Write 写入一个 IP 包。
func (d *nativeDevice) Write(p []byte) (int, error) {
	d.bufs[0] = p
	n, err := d.dev.Write(d.bufs, 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, errors.New("device: write: no packet written")
	}
	return len(p), nil
}

// MTU 返回设备 MTU。
func (d *nativeDevice) MTU() int { return d.mtu }

// Name 返回设备接口名。
func (d *nativeDevice) Name() string {
	name, err := d.dev.Name()
	if err != nil {
		return ""
	}
	return name
}

// Close 关闭设备。
func (d *nativeDevice) Close() error { return d.dev.Close() }

// isPermString 兜底识别底层返回的权限错误字符串。
//
// 某些平台/内核版本下,创建 TUN 失败返回的是裸 syscall.EPERM 且被包装成
// 不含 os.ErrPermission 的错误。用字符串匹配作为最后一道识别手段,
// 保证 auto 模式能正确判断"这是权限问题"。
func isPermString(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "access is denied")
}
