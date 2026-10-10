// Package ingress 实现 Limesh 的接入层(L1)。
//
// 接入层是"虚拟网络与本地网络栈之间的接口":它从内核/应用取包交给数据面,
// 并把数据面解出的包投递回内核/应用。它**不加密、不路由**(AGENTS.md 5.1)。
//
// 三种模式(AGENTS.md 12 章):
//
//	tun     内核 TUN 设备,完整三层,需要 CAP_NET_ADMIN
//	no-tun  用户态协议栈(gVisor netstack),零权限,Phase 3 实现
//	auto    优先 tun,失败按能力降级
//
// Phase 1b 只实现 tun 模式的设备侧读写与包搬运;IP 层路由由 node 数据面
// 以"两点直连"的最简方式处理(完整 OSPF 属 Phase 1c)。
package ingress

import (
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/device"
	"github.com/LiStudioorg/limesh/core/packet"
)

// Mode 是接入层的工作模式。
type Mode string

// 接入模式取值。与 docs/CONFIG.md 6.4 的 ingress.mode 一致。
const (
	ModeTUN   Mode = "tun"
	ModeNoTUN Mode = "no-tun"
	ModeAuto  Mode = "auto"
)

// 接入层错误。
var (
	// ErrNotImplemented 表示该模式尚未实现。
	ErrNotImplemented = errors.New("ingress: not implemented")
	// ErrClosed 表示接入层已关闭。
	ErrClosed = errors.New("ingress: closed")
)

// Ingress 是接入层实现的统一接口。
//
// 读写以 **IP 包**为单位(而非 ZCPacket):Outbound 产出的是本机产生的、
// 尚未加密的 IP 包;Deliver 接收的应是已剥离 ZCPacket 头的 IP 包。
// 加密与 ZCPacket 封装由数据面负责,接入层不感知会话。
type Ingress interface {
	// Deliver 把一个来自虚拟网络的 IP 包投递给本机(写入 TUN)。
	Deliver(pkt *packet.ZCPacket) error
	// Outbound 返回本机产生的 IP 包流(从 TUN 读出)。
	Outbound() <-chan *packet.ZCPacket
	// Close 关闭接入层,释放 TUN 设备。重复调用是安全的。
	Close() error
	// Mode 返回实际生效的模式(tun/no-tun)。
	Mode() Mode
}

// Options 是构造接入层的参数。
type Options struct {
	// Config 是配置快照,必填。
	Config *config.Config
	// Logger 是日志实例;为空时使用 zap.NewNop()。
	Logger *zap.Logger
	// Device 允许注入自定义设备实现(测试用途)。
	// 为空时按配置打开真实 TUN 设备。
	Device device.Device
}

// New 按配置选择并构造接入层。
//
// 模式判定(AGENTS.md 12.2):
//   - tun:直接打开 TUN,失败原样返回;
//   - no-tun:Phase 3 实现,当前返回 ErrNotImplemented;
//   - auto:优先 TUN;权限不足时打印明确的降级提示后返回错误
//     (no-tun 未实现,无法真正降级,但提示必须可见,禁止静默失败)。
func New(opts Options) (Ingress, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("ingress: new: config must not be nil")
	}
	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}

	mode := Mode(opts.Config.Ingress.Mode)
	switch mode {
	case ModeTUN:
		return NewTUN(opts)

	case ModeNoTUN:
		return nil, fmt.Errorf("ingress: mode %q: %w (planned for Phase 3)", ModeNoTUN, ErrNotImplemented)

	case ModeAuto:
		ing, err := NewTUN(opts)
		if err == nil {
			return ing, nil
		}
		// 只有权限类错误才提示降级;其它错误(设备名非法等)直接返回。
		if device.IsPermissionError(err) {
			log.Warn("no permission to open TUN; no-tun fallback not available yet",
				zap.String("mode", "auto"))
			log.Info("hint: grant CAP_NET_ADMIN or run as root to enable full TUN mode",
				zap.String("capability", "cap_net_admin,cap_net_raw"))
		}
		return nil, err

	default:
		return nil, fmt.Errorf("ingress: unknown mode %q", opts.Config.Ingress.Mode)
	}
}

// activeIngress 实现 Mode() 之外的公共状态,供各模式复用。
type activeIngress struct {
	mode  Mode
	outCh chan *packet.ZCPacket
	log   *zap.Logger

	mu     sync.Mutex
	closed bool
}

// Outbound 返回 IP 包流。
func (a *activeIngress) Outbound() <-chan *packet.ZCPacket { return a.outCh }

// Mode 返回实际生效的模式。
func (a *activeIngress) Mode() Mode { return a.mode }

// isClosed 报告是否已关闭。
func (a *activeIngress) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// markClosed 标记关闭,返回是否是首次关闭。
func (a *activeIngress) markClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return false
	}
	a.closed = true
	return true
}
