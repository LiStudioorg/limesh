//go:build linux

package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// openTUN 在 Linux 上创建 TUN 设备。
//
// 需要 CAP_NET_ADMIN(或 root)以及 /dev/net/tun 存在。设备名留空时由
// wireguard-go 使用默认名(tun%d);本项目通常传入 "mesh0"。
func openTUN(cfg Config) (Device, error) {
	dev, err := tun.CreateTUN(cfg.Name, cfg.MTU)
	if err != nil {
		return nil, wrapOpenError(cfg.Name, err)
	}
	return newNativeDevice(dev, cfg.MTU)
}

// fromFDTUN 从已打开的 TUN 文件描述符构造设备(移动端/外部管理场景预留)。
func fromFDTUN(fd int, mtu int) (Device, error) {
	dev, _, err := tun.CreateUnmonitoredTUNFromFD(fd)
	if err != nil {
		return nil, fmt.Errorf("device: from fd %d: %w", fd, err)
	}
	return newNativeDevice(dev, mtu)
}

// wrapOpenError 归一打开错误:权限类错误附加修复指引。
func wrapOpenError(name string, err error) error {
	if permErr := wrapPermissionError(err); permErr != err {
		return permErr
	}
	return fmt.Errorf("device: open TUN %q: %w", name, err)
}
