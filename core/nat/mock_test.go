package nat

import (
	"net"
	"sync"
	"testing"

	"github.com/pion/stun/v2"
	"github.com/stretchr/testify/require"
)

// mockSTUN 是一个最小的 STUN 服务器,用于测试。
//
// 它对 Binding Request 回复预设的 XOR-MAPPED-ADDRESS,使测试不依赖外部网络。
type mockSTUN struct {
	conn *net.UDPConn

	mu      sync.Mutex
	mapped  *stun.XORMappedAddress
	delay   bool // 是否完全不应答(用于超时测试)
	class   stun.MessageClass
	respond bool
}

// newMockSTUN 启动一个 mock STUN 服务器,返回其地址(host:port)。
func newMockSTUN(t *testing.T, mapped *stun.XORMappedAddress) (*mockSTUN, string) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	m := &mockSTUN{conn: conn, mapped: mapped, respond: true, class: stun.ClassSuccessResponse}
	go m.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return m, conn.LocalAddr().String()
}

// serve 处理入站 STUN 请求。
func (m *mockSTUN) serve() {
	buf := make([]byte, 1500)
	for {
		n, addr, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := req.Decode(); err != nil {
			continue
		}
		if req.Type != stun.BindingRequest {
			continue
		}

		m.mu.Lock()
		respond := m.respond
		mapped := m.mapped
		class := m.class
		m.mu.Unlock()

		if !respond {
			continue
		}

		resp, err := stun.Build(
			stun.NewTransactionIDSetter(req.TransactionID),
			stun.NewType(stun.MethodBinding, class),
		)
		if err != nil {
			continue
		}
		if class == stun.ClassSuccessResponse && mapped != nil {
			if err := mapped.AddTo(resp); err != nil {
				continue
			}
		}
		_, _ = m.conn.WriteToUDP(resp.Raw, addr)
	}
}

// setMapped 修改返回的映射地址(用于探测中途改变行为的场景)。
func (m *mockSTUN) setMapped(addr *stun.XORMappedAddress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mapped = addr
}

// setRespond 控制是否应答。
func (m *mockSTUN) setRespond(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.respond = v
}

// v4 构造一个 IPv4 XORMappedAddress。
func v4(ip string, port int) *stun.XORMappedAddress {
	return &stun.XORMappedAddress{IP: net.ParseIP(ip).To4(), Port: port}
}
