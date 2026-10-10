package ingress

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/device"
	"github.com/LiStudioorg/limesh/core/packet"
)

// outboundBuffer 是 TUN → 数据面方向队列的容量。
//
// 取一个较大的值以吸收突发:队列满时按 TUN 语义丢弃新包,而不是阻塞
// 读循环——阻塞读循环会让内核缓冲迅速写满并开始丢包,行为更难预测。
const outboundBuffer = 1024

// mtuBuffer 是单个 IP 包读缓冲的容量上限。IPv4/IPv6 报文理论上限 65535。
const mtuBuffer = 65535

// TUNIngress 是基于内核 TUN 设备的接入层实现。
//
// 它运行一个读循环,把 TUN 上的 IP 包封装为 ZCPacket 送入 Outbound 通道;
// Deliver 则把数据面解出的 IP 包写回 TUN。
type TUNIngress struct {
	activeIngress

	dev device.Device
	cfg *configView

	wg     sync.WaitGroup
	doneCh chan struct{}
}

// configView 是接入层对配置的最小只读视图。
type configView struct {
	networkName string
	deviceName  string
	mtu         int
}

// NewTUN 构造一个 TUN 接入层并启动其读循环。
//
// 设备来源:Options.Device 非空时使用注入的设备(测试),否则按配置打开
// 真实 TUN 设备。打开失败(含权限不足)时返回 device 层归一后的错误。
func NewTUN(opts Options) (*TUNIngress, error) {
	cfg := opts.Config

	name := cfg.EffectiveTUNName()
	mtu := cfg.Network.MTU

	dev := opts.Device
	if dev == nil {
		opened, err := device.Open(device.Config{Name: name, MTU: mtu})
		if err != nil {
			return nil, fmt.Errorf("ingress: open tun %q: %w", name, err)
		}
		dev = opened
	}

	t := &TUNIngress{
		activeIngress: activeIngress{
			mode:  ModeTUN,
			outCh: make(chan *packet.ZCPacket, outboundBuffer),
			log:   opts.Logger,
		},
		dev:    dev,
		doneCh: make(chan struct{}),
		cfg: &configView{
			networkName: cfg.Network.Name,
			deviceName:  dev.Name(),
			mtu:         dev.MTU(),
		},
	}
	if t.log == nil {
		t.log = zap.NewNop()
	}

	t.wg.Add(1)
	go t.readLoop()

	return t, nil
}

// DeviceName 返回底层 TUN 设备名,供日志与 CLI 展示。
func (t *TUNIngress) DeviceName() string { return t.cfg.deviceName }

// MTU 返回底层设备 MTU。
func (t *TUNIngress) MTU() int { return t.cfg.mtu }

// readLoop 从 TUN 读 IP 包并封装为 ZCPacket 送入 Outbound,直到设备关闭。
func (t *TUNIngress) readLoop() {
	defer t.wg.Done()
	defer close(t.outCh)

	buf := make([]byte, mtuBuffer)
	for {
		n, err := t.dev.Read(buf)
		if err != nil {
			if !t.isClosed() {
				t.log.Debug("tun read loop stopped", zap.Error(err))
			}
			return
		}
		if n <= 0 {
			continue
		}

		// packet.New 会拷贝载荷,因此可以安全复用 buf 读下一个包。
		pkt := packet.New(buf[:n])
		pkt.SetType(packet.TypeData)

		select {
		case t.outCh <- pkt:
		case <-t.doneCh:
			return
		default:
			// 缓冲满:按 TUN 语义丢弃,避免阻塞读循环。
			t.log.Debug("ingress outbound buffer full, dropping packet",
				zap.Int("len", n))
		}
	}
}

// Deliver 把一个来自虚拟网络的 IP 包写回 TUN。
//
// 入参是承载密文的 ZCPacket;接入层只取其中的明文载荷(IP 包)。
func (t *TUNIngress) Deliver(pkt *packet.ZCPacket) error {
	if pkt == nil {
		return fmt.Errorf("ingress: deliver: nil packet")
	}
	if t.isClosed() {
		return fmt.Errorf("ingress: deliver: %w", ErrClosed)
	}

	payload := pkt.Payload()
	if len(payload) == 0 {
		return nil
	}
	if _, err := t.dev.Write(payload); err != nil {
		return fmt.Errorf("ingress: deliver: write tun: %w", err)
	}
	return nil
}

// Close 关闭接入层与底层设备。重复调用是安全的。
func (t *TUNIngress) Close() error {
	if !t.markClosed() {
		return nil
	}
	close(t.doneCh)

	// 关闭设备使读循环解除阻塞并退出。
	err := t.dev.Close()
	t.wg.Wait()
	return err
}
