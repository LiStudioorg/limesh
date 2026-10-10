//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package device

import "fmt"

// openTUN 在未支持 TUN 的平台上返回明确错误。
//
// 覆盖 linux/darwin/windows/freebsd/openbsd 之外的平台(如 netbsd、
// dragonfly、solaris、js/wasm 等)。返回 ErrUnsupported 而非 panic,
// 使上层 auto 模式可以据此降级或报错。
func openTUN(cfg Config) (Device, error) {
	return nil, fmt.Errorf("device: open TUN %q: %w", cfg.Name, ErrUnsupported)
}

// fromFDTUN 在未支持 TUN 的平台上返回明确错误。
func fromFDTUN(fd int, mtu int) (Device, error) {
	return nil, fmt.Errorf("device: from fd %d: %w", fd, ErrUnsupported)
}
