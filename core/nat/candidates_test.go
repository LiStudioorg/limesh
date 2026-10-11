package nat

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/config"
)

// newTestCollector 构造一个不依赖 STUN 的收集器。
func newTestCollector(t *testing.T, mutate func(*config.Config)) *Collector {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	c, err := NewCollector(CollectorOptions{Config: cfg})
	require.NoError(t, err)
	return c
}

// TestNewCollectorRejectsNilConfig 验证 nil 配置被拒绝。
func TestNewCollectorRejectsNilConfig(t *testing.T) {
	_, err := NewCollector(CollectorOptions{})
	require.Error(t, err)
}

// TestCollectHostCandidates 验证 host 候选被收集且排除了 loopback。
func TestCollectHostCandidates(t *testing.T) {
	c := newTestCollector(t, nil)

	cands, err := c.Collect(context.Background())
	require.NoError(t, err)

	for _, cand := range cands {
		assert.Equal(t, CandidateHost, cand.Type)
		assert.False(t, cand.Addr.Addr().IsLoopback(), "不应包含 loopback 地址")
		assert.NotZero(t, cand.Addr.Port(), "候选地址应带端口")
	}
}

// TestCollectHostPriority 验证候选按优先级降序排序。
func TestCollectHostPriority(t *testing.T) {
	c := newTestCollector(t, nil)

	cands, err := c.Collect(context.Background())
	require.NoError(t, err)

	for i := 1; i < len(cands); i++ {
		assert.GreaterOrEqual(t, cands[i-1].Priority, cands[i].Priority,
			"候选应按优先级降序")
	}
}

// TestPortOfFromConfig 验证端口从配置的 listen 推导。
func TestPortOfFromConfig(t *testing.T) {
	c := newTestCollector(t, func(cfg *config.Config) {
		cfg.Listen = []config.ListenConfig{{Addr: "udp://0.0.0.0:34567"}}
	})
	assert.Equal(t, uint16(34567), c.portOf())
}

// TestPortOfExplicitOverride 验证显式 Port 覆盖配置。
func TestPortOfExplicitOverride(t *testing.T) {
	cfg := config.Default()
	c, err := NewCollector(CollectorOptions{Config: cfg, Port: 12345})
	require.NoError(t, err)
	assert.Equal(t, uint16(12345), c.portOf())
}

// TestPortOfDefault 验证无法解析时回退默认端口。
func TestPortOfDefault(t *testing.T) {
	c := newTestCollector(t, func(cfg *config.Config) {
		cfg.Listen = []config.ListenConfig{{Addr: "garbage"}}
	})
	assert.Equal(t, uint16(config.DefaultPort), c.portOf())
}

// TestPriorityOrdering 验证类型偏好:host > srflx > relay。
func TestPriorityOrdering(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.1")
	host := priority(CandidateHost, ip)
	srflx := priority(CandidateSrflx, ip)
	relay := priority(CandidateRelay, ip)

	assert.Greater(t, host, srflx, "host 应优先于 srflx")
	assert.Greater(t, srflx, relay, "srflx 应优先于 relay")
	assert.Equal(t, uint32(256-componentRTP), host&0xFF, "component 位应正确")
}

// TestPriorityIPv4VsIPv6 验证 IPv6 全球单播优先于 IPv4 私有。
func TestPriorityIPv4VsIPv6(t *testing.T) {
	v6 := netip.MustParseAddr("2001:db8::1")
	v4priv := netip.MustParseAddr("192.168.1.1")

	// 同为 host 类型,比较 local_pref。
	p6 := priority(CandidateHost, v6)
	p4 := priority(CandidateHost, v4priv)
	assert.Greater(t, p6, p4, "IPv6 全球单播应优先于 IPv4 私有")
}

// TestMatchSkipPrefix 验证网卡名前缀匹配。
func TestMatchSkipPrefix(t *testing.T) {
	skip := []string{"docker", "veth", "br-", "", "tun"}
	assert.True(t, matchSkipPrefix("docker0", skip))
	assert.True(t, matchSkipPrefix("veth123", skip))
	assert.True(t, matchSkipPrefix("br-abc", skip))
	assert.True(t, matchSkipPrefix("tun0", skip))
	assert.False(t, matchSkipPrefix("eth0", skip))
	assert.False(t, matchSkipPrefix("", skip))
}

// TestCollectSrflx 验证 STUN 反射候选被加入且类型为 srflx。
func TestCollectSrflx(t *testing.T) {
	_, a1 := newMockSTUN(t, v4("203.0.113.77", 45678))
	_, a2 := newMockSTUN(t, v4("203.0.113.77", 45678))

	det, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer det.Close()
	det.SetTimeout(time.Second)

	cfg := config.Default()
	c, err := NewCollector(CollectorOptions{Config: cfg, Detector: det})
	require.NoError(t, err)

	cands, err := c.Collect(context.Background())
	require.NoError(t, err)

	var found *Candidate
	for i := range cands {
		if cands[i].Type == CandidateSrflx {
			found = &cands[i]
			break
		}
	}
	require.NotNil(t, found, "应包含 srflx 候选")
	assert.Equal(t, "203.0.113.77:45678", found.Addr.String())
	assert.NotEmpty(t, found.Source)
}

// TestCollectSrflxFailureIsSilent 验证 STUN 失败不阻断 host 候选收集。
func TestCollectSrflxFailureIsSilent(t *testing.T) {
	m1, a1 := newMockSTUN(t, nil)
	m1.setRespond(false)
	m2, a2 := newMockSTUN(t, nil)
	m2.setRespond(false)

	det, err := NewDetector([]string{a1, a2})
	require.NoError(t, err)
	defer det.Close()
	det.SetTimeout(200 * time.Millisecond)

	cfg := config.Default()
	c, err := NewCollector(CollectorOptions{Config: cfg, Detector: det})
	require.NoError(t, err)

	cands, err := c.Collect(context.Background())
	require.NoError(t, err, "STUN 失败不应导致收集返回错误")

	for _, cand := range cands {
		assert.NotEqual(t, CandidateSrflx, cand.Type, "失败时不应有 srflx 候选")
	}
}

// TestCandidateString 验证候选字符串渲染。
func TestCandidateString(t *testing.T) {
	c := Candidate{
		Type:     CandidateHost,
		Addr:     netip.MustParseAddrPort("192.168.1.1:11010"),
		Priority: 2130706431,
	}
	s := c.String()
	assert.Contains(t, s, "host")
	assert.Contains(t, s, "192.168.1.1:11010")
}
