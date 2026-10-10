//go:build windows

package device

import (
	"fmt"

	"golang.zx2c4.com/wireguard/tun"
)

// openTUN 在 Windows 上创建 Wintun 设备。
//
// 需要以管理员身份运行,且需随程序分发 wintun.dll。设备名可直接指定
// (如 "mesh"),不指定时由 Wintun 生成。
func openTUN(cfg Config) (Device, error) {
	name := cfg.Name
	if name == "" {
		// Wintun 需要一个名称;留空时使用稳定默认名,便于后续 ipconfig 引用。
		name = "mesh"
	}
	dev, err := tun.CreateTUN(name, cfg.MTU)
	if err != nil {
		return nil, wrapOpenError(name, err)
	}
	return newNativeDevice(dev, cfg.MTU)
}

// fromFDTUN 在 Windows 上不适用(设备为句柄而非 fd)。
func fromFDTUN(fd int, mtu int) (Device, error) {
	return nil, fmt.Errorf("device: from fd on windows: %w", ErrUnsupported)
}

// wrapOpenError 归一打开错误:权限类错误附加修复指引。
func wrapOpenError(name string, err error) error {
	if permErr := wrapPermissionError(err); permErr != err {
		return permErr
	}
	return fmt.Errorf("device: open TUN %q: %w", name, err)
}
