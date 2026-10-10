//go:build darwin

package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// openTUN 在 macOS 上创建 utun 设备。
//
// macOS 的 utun 名字由系统分配(utun0、utun1……),不能指定,因此忽略
// cfg.Name。需要 root 权限。
func openTUN(cfg Config) (Device, error) {
	// 忽略 cfg.Name:utun 的名称由内核分配。
	dev, err := tun.CreateTUN("utun", cfg.MTU)
	if err != nil {
		return nil, wrapOpenError(cfg.Name, err)
	}
	return newNativeDevice(dev, cfg.MTU)
}

// fromFDTUN 在 macOS 上尚不支持从 fd 构造。
func fromFDTUN(fd int, mtu int) (Device, error) {
	return nil, fmt.Errorf("device: from fd on darwin: %w", ErrUnsupported)
}

// wrapOpenError 归一打开错误:权限类错误附加修复指引。
func wrapOpenError(name string, err error) error {
	if permErr := wrapPermissionError(err); permErr != err {
		return permErr
	}
	return fmt.Errorf("device: open TUN (utun): %w", err)
}
