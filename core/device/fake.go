package device

import (
	"io"
	"sync"
)

// FakeDevice 是内存中的假 TUN 设备,用于测试。
//
// 它让 Ingress 与数据面可以在无 root、无真实网卡的 CI 环境中被完整测试:
// 向 Inbound 写入会被 Read 读出,Write 写入的包可从 Outbound 读到。
//
// 字段直接暴露以便测试驱动。FakeDevice 本身并发安全。
type FakeDevice struct {
	// Inbound 是"从内核流向本进程"的 IP 包队列,由 Read 消费。
	Inbound chan []byte
	// Outbound 是"本进程写入内核"的 IP 包队列,由 Write 生产。
	Outbound chan []byte

	// mtu 是设备 MTU。
	mtu int
	// name 是设备名。
	name string

	mu     sync.Mutex
	closed bool
	// readErr/writeErr 可注入以覆盖错误路径。
	readErr  error
	writeErr error
	// writeCount 记录写入次数。
	writeCount int
}

// NewFakeDevice 创建一个假设备,队列带缓冲以适配突发。
func NewFakeDevice(name string, mtu int) *FakeDevice {
	if mtu <= 0 {
		mtu = 1380
	}
	return &FakeDevice{
		Inbound:  make(chan []byte, 64),
		Outbound: make(chan []byte, 64),
		mtu:      mtu,
		name:     name,
	}
}

// Read 从 Inbound 取出一个包。
//
// 传入的 buf 必须足够大;包被复制进 buf。设备关闭后返回 io.EOF。
func (d *FakeDevice) Read(buf []byte) (int, error) {
	d.mu.Lock()
	closed := d.closed
	err := d.readErr
	d.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}

	pkt, ok := <-d.Inbound
	if !ok {
		return 0, io.EOF
	}
	n := copy(buf, pkt)
	return n, nil
}

// Write 把一个包放入 Outbound。
func (d *FakeDevice) Write(p []byte) (int, error) {
	d.mu.Lock()
	closed := d.closed
	err := d.writeErr
	d.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}

	cp := make([]byte, len(p))
	copy(cp, p)

	d.mu.Lock()
	d.writeCount++
	d.mu.Unlock()

	select {
	case d.Outbound <- cp:
	default:
		// 与真实 TUN 一致:缓冲满时丢弃,避免阻塞调用方。
	}
	return len(p), nil
}

// MTU 返回设备 MTU。
func (d *FakeDevice) MTU() int { return d.mtu }

// Name 返回设备名。
func (d *FakeDevice) Name() string { return d.name }

// Close 关闭设备,使后续 Read/Write 返回错误。
func (d *FakeDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	close(d.Inbound)
	return nil
}

// Closed 报告设备是否已关闭。
func (d *FakeDevice) Closed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// WriteCount 返回累计写入次数。
func (d *FakeDevice) WriteCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeCount
}

// SetReadErr 注入读错误(返回 nil 清除)。
func (d *FakeDevice) SetReadErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readErr = err
}

// SetWriteErr 注入写错误(返回 nil 清除)。
func (d *FakeDevice) SetWriteErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeErr = err
}
