//go:build freebsd

package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// openTUN 在 FreeBSD 上创建 tun 设备。
//
// 设备名通常形如 tun0;留空时由系统分配。需要 root 权限以及可用的 tun 驱动。
func openTUN(cfg Config) (Device, error) {
	dev, err := tun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, wrapOpenError(cfg.Name, err)
	}
	return newNativeDevice(dev, cfg.MTU)
}

// fromFDTUN 在 FreeBSD 上尚不支持从 fd 构造。
func fromFDTUN(fd int, mtu int) (Device, error) {
	return nil, fmt.Errorf("device: from fd on freebsd: %w", ErrUnsupported)
}

// wrapOpenError 归一打开错误:权限类错误附加修复指引。
func wrapOpenError(name string, err error) error {
	if permErr := wrapPermissionError(err); permErr != err {
		return permErr
	}
	return fmt.Errorf("device: open TUN %q: %w", name, err)
}
