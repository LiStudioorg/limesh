// Package device 提供跨平台的 TUN 虚拟网卡抽象。
//
// 职责边界:本包只负责**打开/读写/关闭一个 TUN 设备**,不做加密、不做路由、
// 不感知虚拟网络。它把 wireguard-go/tun 的批量读写接口归一为更简单的
// 单包 Read/Write,使上层 Ingress 实现不必处理批处理细节。
//
// 平台差异(见 AGENTS.md 12 章与 13 章):
//
//	Linux   设备名 mesh0(可用 ip link 看到)  需要 CAP_NET_ADMIN
//	macOS   设备名 utun*(系统分配,不可指定)    需要 root
//	Windows 设备名 mesh(Wintun 驱动)           需要管理员
//	FreeBSD 设备名 tun*(系统分配)               需要 root
//
// 其它平台返回明确错误(见 tun_stub.go)。核心库保持 CGO_ENABLED=0 可编译。
package device

import (
	"errors"
	"fmt"
	"os"
)

// Device 是一个已打开的 TUN 设备。
//
// 读写以**单个 IP 包**为单位:Read 返回一个完整的 IP 包,Write 写入一个
// 完整的 IP 包。底层 wireguard-go/tun 使用批量接口,本包负责适配。
type Device interface {
	// Read 读取一个 IP 包到 buf,返回包长度。buf 太小则包被截断。
	Read(buf []byte) (int, error)
	// Write 写入一个 IP 包。实现不得在返回后继续引用 p。
	Write(p []byte) (int, error)
	// MTU 返回设备 MTU。
	MTU() int
	// Name 返回设备接口名。
	Name() string
	// Close 关闭设备。重复调用是安全的。
	Close() error
}

// Config 是打开 TUN 设备的参数。
type Config struct {
	// Name 是期望的设备名,如 "mesh0"。
	//
	// 留空表示由系统分配(macOS 必须留空,否则会退化为系统默认命名)。
	Name string
	// MTU 是设备 MTU;为 0 时由平台使用合理默认值。
	MTU int
}

// 设备相关错误。
var (
	// ErrPermission 表示打开 TUN 设备时权限不足。
	ErrPermission = errors.New("device: permission denied opening TUN")
	// ErrUnsupported 表示当前平台不支持创建 TUN 设备。
	ErrUnsupported = errors.New("device: TUN not supported on this platform")
)

// Open 打开(创建)一个 TUN 设备。
//
// 桌面端使用:进程自己创建 TUN。失败时,权限类错误会被归一为 ErrPermission
// 并附带按平台的修复指引;其它错误原样返回。
//
// 成功后调用方负责 Close。设备名:Linux 可用 cfg.Name(如 "mesh0");
// macOS 必须留空(cfg.Name 非空会被忽略,由系统分配 utun*)。
func Open(cfg Config) (Device, error) {
	if cfg.MTU < 0 {
		return nil, fmt.Errorf("device: open: negative mtu %d", cfg.MTU)
	}
	return openTUN(cfg)
}

// FromFD 从已打开的文件描述符构造 TUN 设备。
//
// 供移动端(Android VPNService / iOS NetworkExtension)与由外部管理 TUN
// 的场景使用:fd 由系统或上层传入,本进程不负责创建。mtu 传 0 时从设备读取。
//
// 目前 Linux 支持该入口;其它平台返回 ErrUnsupported。
func FromFD(fd int, mtu int) (Device, error) {
	if fd < 0 {
		return nil, fmt.Errorf("device: from fd: invalid fd %d", fd)
	}
	return fromFDTUN(fd, mtu)
}

// permissionHint 是权限不足时打印给用户的、按平台的修复指引。
//
// 它必须包含:失败原因、每个平台的具体修复方法。静默失败或只报
// "permission denied" 会让用户无从下手(AGENTS.md 12.2 的降级提示要求)。
const permissionHint = `permission denied opening TUN device

Fix:
  Linux:   sudo setcap cap_net_admin,cap_net_raw+ep $(which mesh)
           then: mesh node ...
  macOS:   sudo mesh node ...
  Windows: run as Administrator
  Docker:  add --cap-add NET_ADMIN --device /dev/net/tun
  K8s:     securityContext.capabilities.add: ["NET_ADMIN", "NET_RAW"]`

// wrapPermissionError 把底层权限错误归一为带修复指引的 ErrPermission。
//
// 识别依据:os.ErrPermission,以及 Linux 内核在缺少 CAP_NET_ADMIN 时
// 返回的 EPERM(通常在 os.PathError 中表现为 "operation not permitted")。
func wrapPermissionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrPermission) || isPermString(err) {
		return fmt.Errorf("%w: %w\n%s", ErrPermission, err, permissionHint)
	}
	return err
}

// IsPermissionError 报告错误是否表示"缺少打开 TUN 的权限"。
//
// 供 auto 模式判断是否应打印降级提示:只有权限类错误才提示降级,
// 其它错误(如设备名非法)应直接返回。
func IsPermissionError(err error) bool {
	return err != nil && errors.Is(err, ErrPermission)
}
