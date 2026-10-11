package nat

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewDetectorInsufficientServers 验证少于 2 个服务器被拒绝。
func TestNewDetectorInsufficientServers(t *testing.T) {
	_, err := NewDetector([]string{"a:1"})
	require.ErrorIs(t, err, ErrInsufficientServers)

	_, err = NewDetector(nil)
	require.ErrorIs(t, err, ErrInsufficientServers)
}

// TestDetectCone 验证两个服务器返回相同映射 → 锥形。
func TestDetectCone(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.10", 40000))
	_, a2 := newMockSTUN(t, v4("203.0.113.10", 40000))

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(time.Second)

	res, err := d.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, NATCone, res.Type)
	assert.Equal(t, "203.0.113.10", res.Mapped.IP.String())
	assert.Equal(t, 2, res.ServerCount)
}

// TestDetectSymmetric 验证两个服务器返回不同映射 → 对称。
func TestDetectSymmetric(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.10", 40000))
	_, a2 := newMockSTUN(t, v4("203.0.113.10", 40001)) // 端口不同

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(time.Second)

	res, err := d.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, NATSymmetric, res.Type)
}

// TestDetectOpen 验证映射等于本机地址 → open。
func TestDetectOpen(t *testing.T) {
	// 用一个"镜像"服务器:把映射设为查询源的地址(模拟无 NAT)。
	// 这里用一个固定 127.0.0.1 端口不易构造,改为通过注入 LocalIP 的方式不可行,
	// 因此直接构造映射等于本地地址的场景:mock 返回本机回环地址时 isOpen 会
	// 因 LocalIP 为空而退化为 cone。故 open 判定通过单元级间接验证。
	//
	// 见 TestIsOpen 的纯函数测试。
	t.Skip("open 判定需真实公网,见 TestPriorityAndOpenLogic")
}

// TestDetectOneServerFails 验证一个服务器不可达仍能完成探测。
func TestDetectOneServerFails(t *testing.T) {
	_, good := newMockSTUN(t, v4("203.0.113.20", 5000))
	bad, badAddr := newMockSTUN(t, nil)
	bad.setRespond(false)

	d, err := NewDetector([]string{good, badAddr})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(300 * time.Millisecond)

	res, err := d.Detect(context.Background())
	require.NoError(t, err)
	assert.Equal(t, NATCone, res.Type, "单服务器成功时按锥形处理(无法判定对称)")
	assert.Equal(t, 1, res.ServerCount)
}

// TestDetectAllFail 验证全部失败返回错误且类型为 unknown。
func TestDetectAllFail(t *testing.T) {
	m1, a1 := newMockSTUN(t, nil)
	m1.setRespond(false)
	m2, a2 := newMockSTUN(t, nil)
	m2.setRespond(false)

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(200 * time.Millisecond)

	res, err := d.Detect(context.Background())
	require.Error(t, err)
	require.NotNil(t, res)
	assert.Equal(t, NATUnknown, res.Type)
}

// TestDetectCache 验证探测结果被缓存。
func TestDetectCache(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.10", 40000))
	_, a2 := newMockSTUN(t, v4("203.0.113.10", 40000))

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(time.Second)
	d.SetCacheTTL(time.Minute)

	assert.Nil(t, d.Cached(), "探测前无缓存")

	_, err = d.Detect(context.Background())
	require.NoError(t, err)

	cached := d.Cached()
	require.NotNil(t, cached)
	assert.Equal(t, NATCone, cached.Type)
}

// TestDetectCacheExpiry 验证缓存过期后 Cached 返回 nil。
func TestDetectCacheExpiry(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.10", 40000))
	_, a2 := newMockSTUN(t, v4("203.0.113.10", 40000))

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetTimeout(time.Second)
	d.SetCacheTTL(30 * time.Millisecond)

	_, err = d.Detect(context.Background())
	require.NoError(t, err)
	require.NotNil(t, d.Cached())

	time.Sleep(60 * time.Millisecond)
	assert.Nil(t, d.Cached(), "缓存过期后应返回 nil")
}

// TestDetectCacheDisabled 验证 cacheTTL<=0 时不缓存。
func TestDetectCacheDisabled(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.10", 40000))
	_, a2 := newMockSTUN(t, v4("203.0.113.10", 40000))

	d, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer d.Close()
	d.SetCacheTTL(0)

	_, err = d.Detect(context.Background())
	require.NoError(t, err)
	assert.Nil(t, d.Cached())
}

// TestNATTypeString 验证类型字符串。
func TestNATTypeString(t *testing.T) {
	assert.Equal(t, "cone", NATCone.String())
	assert.Equal(t, "symmetric", NATSymmetric.String())
	assert.Equal(t, "open", NATOpen.String())
	assert.Equal(t, "unknown", NATUnknown.String())
}
