package nat

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewSTUNClientNoServers 验证空服务器列表被拒绝。
func TestNewSTUNClientNoServers(t *testing.T) {
	_, err := NewSTUNClient(nil)
	require.ErrorIs(t, err, ErrNoServer)
}

// TestSTUNQuery 验证单服务器查询返回正确映射与 RTT。
func TestSTUNQuery(t *testing.T) {
	_, addr := newMockSTUN(t, v4("203.0.113.5", 45231))

	c, err := NewSTUNClient([]string{addr})
	require.NoError(t, err)
	defer c.Close()
	c.SetTimeout(2 * time.Second)

	m, err := c.Query(context.Background(), addr)
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("203.0.113.5"), m.IP)
	assert.Equal(t, uint16(45231), m.Port)
	assert.Equal(t, addr, m.Server)
	assert.Greater(t, m.RTT, time.Duration(0))
}

// TestSTUNQueryStunScheme 验证 stun: 前缀被接受。
func TestSTUNQueryStunScheme(t *testing.T) {
	_, addr := newMockSTUN(t, v4("198.51.100.7", 1234))

	c, err := NewSTUNClient([]string{"stun:" + addr})
	require.NoError(t, err)
	defer c.Close()

	m, err := c.Query(context.Background(), "stun:"+addr)
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7", m.IP.String())
}

// TestSTUNQueryTimeout 验证无响应时超时。
func TestSTUNQueryTimeout(t *testing.T) {
	mock, addr := newMockSTUN(t, v4("203.0.113.5", 1))
	mock.setRespond(false)

	c, err := NewSTUNClient([]string{addr})
	require.NoError(t, err)
	defer c.Close()
	c.SetTimeout(150 * time.Millisecond)

	_, err = c.Query(context.Background(), addr)
	require.Error(t, err)
}

// TestSTUNQueryAnyFirstSuccess 验证 QueryAny 返回首个成功结果。
func TestSTUNQueryAnyFirstSuccess(t *testing.T) {
	_, good := newMockSTUN(t, v4("203.0.113.9", 5555))
	dead, deadAddr := newMockSTUN(t, v4("0.0.0.0", 1))
	dead.setRespond(false)

	c, err := NewSTUNClient([]string{deadAddr, good})
	require.NoError(t, err)
	defer c.Close()
	c.SetTimeout(500 * time.Millisecond)

	m, err := c.QueryAny(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "203.0.113.9", m.IP.String())
	assert.Equal(t, uint16(5555), m.Port)
}

// TestSTUNQueryAnyAllFail 验证全部失败时返回错误。
func TestSTUNQueryAnyAllFail(t *testing.T) {
	m1, a1 := newMockSTUN(t, nil)
	a1 += "" // 保留地址变量
	m1.setRespond(false)
	m2, a2 := newMockSTUN(t, nil)
	m2.setRespond(false)

	c, err := NewSTUNClient([]string{a1, a2})
	require.NoError(t, err)
	defer c.Close()
	c.SetTimeout(150 * time.Millisecond)

	_, err = c.QueryAny(context.Background())
	require.Error(t, err)
}

// TestSTUNSocketReuse 验证多次查询复用同一 socket(本地端口不变)。
func TestSTUNSocketReuse(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.1", 100))
	_, a2 := newMockSTUN(t, v4("203.0.113.1", 100))

	c, err := NewSTUNClient([]string{a1, a2})
	require.NoError(t, err)
	defer c.Close()

	_, err = c.Query(context.Background(), a1)
	require.NoError(t, err)
	port1 := c.LocalPort()

	_, err = c.Query(context.Background(), a2)
	require.NoError(t, err)
	port2 := c.LocalPort()

	assert.NotZero(t, port1)
	assert.Equal(t, port1, port2, "所有查询必须复用同一本地端口")
}

// TestSTUNResolveError 验证非法服务器地址报错。
func TestSTUNResolveError(t *testing.T) {
	c, err := NewSTUNClient([]string{"bad:addr:with:colons"})
	require.NoError(t, err)
	defer c.Close()

	_, err = c.Query(context.Background(), "bad:addr:with:colons")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve")
}

// TestSTUNServersAccessor 验证 Servers 返回副本。
func TestSTUNServersAccessor(t *testing.T) {
	c, err := NewSTUNClient([]string{"a:1", "b:2"})
	require.NoError(t, err)
	s := c.Servers()
	require.Len(t, s, 2)
	s[0] = "mutated"
	assert.Equal(t, "a:1", c.Servers()[0], "应返回副本")
}

// TestMappedAddrAccessors 验证 MappedAddr 的访问器。
func TestMappedAddrAccessors(t *testing.T) {
	m := &MappedAddr{IP: netip.MustParseAddr("203.0.113.1"), Port: 8080}
	assert.Equal(t, "203.0.113.1:8080", m.String())
	assert.Equal(t, uint16(8080), m.AddrPort().Port())

	var nilM *MappedAddr
	assert.Equal(t, "", nilM.String())
	assert.False(t, nilM.AddrPort().IsValid())
}

// TestSTUNContextCancel 验证 ctx 取消使查询中止。
func TestSTUNContextCancel(t *testing.T) {
	mock, addr := newMockSTUN(t, v4("203.0.113.5", 1))
	mock.setRespond(false)

	c, err := NewSTUNClient([]string{addr})
	require.NoError(t, err)
	defer c.Close()
	c.SetTimeout(5 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = c.Query(ctx, addr)
	require.Error(t, err)
}
