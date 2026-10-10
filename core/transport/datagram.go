package transport

import (
	"net"
	"time"
)

// DatagramConn 是"无连接、按来源地址收发"的可选传输能力。
//
// 三层组网的数据面需要在一个监听端口上同时与多个对端通信:每个对端的身份
// 由 ZCPacket.SrcNodeID 决定,而地址则由数据报的来源决定。Listener/Conn 的
// "每个对端一条连接"模型对这种场景并不合适——它要求先 Accept 才能读,而
// UDP 的入站包在任何 Accept 之前就已到达。
//
// 因此把这种能力单独抽成一个接口:
//   - 传输实现了它,node 数据面就走多对端数据报路径;
//   - 未实现(如未来的连接型传输),node 回退到 Phase 0 的 Listener/Conn 路径。
//
// 接口刻意保持与 net.PacketConn 同构,便于直接复用标准库语义与测试夹具。
type DatagramConn interface {
	// ReadFrom 读取一个数据报,并返回其来源地址。
	//
	// 返回的切片 p 由调用方提供,数据报超出 p 的部分被丢弃(UDP 语义)。
	ReadFrom(p []byte) (n int, addr net.Addr, err error)
	// WriteTo 向 addr 发送一个数据报。实现不得在返回后引用 p。
	WriteTo(p []byte, addr net.Addr) (n int, err error)
	// SetReadDeadline 设置读操作的截止时间;零值表示不设期限。
	SetReadDeadline(t time.Time) error
	// LocalAddr 返回本地监听地址。
	LocalAddr() net.Addr
	// Close 关闭数据报连接。重复调用是安全的。
	Close() error
}

// AsDatagram 尝试把 Listener 视为 DatagramConn。
//
// 传输实现可以选择让 Listen 返回的对象同时满足 Listener 与 DatagramConn;
// 此时数据面应优先使用 DatagramConn。返回 ok 为 false 时调用方回退到
// Listener 路径。
func AsDatagram(l Listener) (DatagramConn, bool) {
	if l == nil {
		return nil, false
	}
	d, ok := l.(DatagramConn)
	return d, ok
}
