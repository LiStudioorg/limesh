//go:build openbsd

package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// openTUN 在 OpenBSD 上创建 tun 设备。
//
// 需要 root 权限。tun 设备的创建需要在 /dev 下有对应的 tunN 节点。
func openTUN(cfg Config) (Device, error) {
	dev, err := tun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, wrapOpenError(cfg.Name, err)
	}
	return newNativeDevice(dev, cfg.MTU)
}

// fromFDTUN 在 OpenBSD 上尚不支持从 fd 构造。
func fromFDTUN(fd int, mtu int) (Device, error) {
	return nil, fmt.Errorf("device: from fd on openbsd: %w", ErrUnsupported)
}

// wrapOpenError 归一打开错误:权限类错误附加修复指引。
func wrapOpenError(name string, err error) error {
	if permErr := wrapPermissionError(err); permErr != err {
		return permErr
	}
	return fmt.Errorf("device: open TUN %q: %w", name, err)
}
