package crypto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveVirtualIPIsDeterministic(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	id := kp.NodeID()

	first := DeriveVirtualIP(id, true)
	for i := 0; i < 100; i++ {
		assert.Equal(t, first, DeriveVirtualIP(id, true), "派生必须是纯函数")
	}
}

func TestDeriveVirtualIPDiffersByNodeID(t *testing.T) {
	a, err := GenerateKeyPair()
	require.NoError(t, err)
	b, err := GenerateKeyPair()
	require.NoError(t, err)

	// 两者的第三、四字节同时相同的概率极低;此处只要求整体不相等。
	assert.NotEqual(t, DeriveVirtualIP(a.NodeID(), true), DeriveVirtualIP(b.NodeID(), true))
}

func TestDeriveVirtualIPv4StaysInSubnet(t *testing.T) {
	// 遍历所有第三字节取值,确认派生结果始终落在 10.126.0.0/16 内,
	// 且避开网络地址、广播地址与网关保留位。
	for third := 0; third < 256; third++ {
		for fourth := 0; fourth < 256; fourth++ {
			id := [NodeIDSize]byte{byte(third), byte(fourth)}
			addr := DeriveVirtualIP(id, true)

			require.True(t, addr.Is4(), "必须返回 IPv4 地址")
			require.True(t, addr.IsValid())

			octets := addr.As4()
			assert.Equal(t, byte(10), octets[0], "第一字节应为 10")
			assert.Equal(t, byte(126), octets[1], "第二字节应为 126")
			assert.Equal(t, byte(third), octets[2], "第三字节应来自 nodeID[0]")

			got := octets[3]
			assert.NotEqual(t, byte(NetworkOctet), got,
				"nodeID=%02x%02x 派生出网络地址", third, fourth)
			assert.NotEqual(t, byte(BroadcastOctet), got,
				"nodeID=%02x%02x 派生出广播地址", third, fourth)
			assert.NotEqual(t, byte(ReservedGatewayOctet), got,
				"nodeID=%02x%02x 派生出网关保留位", third, fourth)
			assert.GreaterOrEqual(t, got, byte(2), "主机地址不应低于 2")
			assert.LessOrEqual(t, got, byte(254), "主机地址不应高于 254")
		}
	}
}

func TestDeriveVirtualIPv4AvoidsReservedOctets(t *testing.T) {
	cases := map[string][NodeIDSize]byte{
		"network .0":     {5, 0},
		"broadcast .255": {5, 255},
		"gateway .1":     {5, 1},
	}

	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			addr := DeriveVirtualIP(id, true)
			octets := addr.As4()

			assert.Equal(t, byte(5), octets[2], "第三字节应保留")
			assert.Equal(t, byte(2), octets[3], "保留位应被环绕到 2")
		})
	}
}

func TestDeriveVirtualIPv6InPrefix(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	addr := DeriveVirtualIP(kp.NodeID(), false)

	require.True(t, addr.Is6(), "应返回 IPv6 地址")
	require.True(t, addr.IsValid())

	prefix := netip.MustParsePrefix("fd00:126:126::/64")
	assert.True(t, prefix.Contains(addr), "%s 应落在 %s 内", addr, prefix)
}

func TestDeriveVirtualIPv6UsesNodeIDFirstEightBytes(t *testing.T) {
	id := [NodeIDSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	addr := DeriveVirtualIP(id, false)
	raw := addr.As16()

	assert.Equal(t, []byte{1, 2, 3, 4, 5, 6, 7, 8}, raw[8:],
		"接口标识应取 nodeID[0:8]")
}

func TestDeriveVirtualIPv6AvoidsZeroInterfaceID(t *testing.T) {
	// 全零接口标识会派生出前缀本身(网段地址),必须规避。
	var id [NodeIDSize]byte
	addr := DeriveVirtualIP(id, false)

	raw := addr.As16()
	assert.NotEqual(t, [8]byte{}, [8]byte(raw[8:]), "接口标识不应全零")
	assert.NotEqual(t, virtualIPv6Prefix.Addr(), addr, "不应等于网段地址")
}

func TestDeriveVirtualIPIsStableAcrossCalls(t *testing.T) {
	// 同一 NodeID 的 v4 与 v6 派生结果应各自稳定且互不相同。
	id := [NodeIDSize]byte{0xAA, 0xBB, 0xCC}
	v4 := DeriveVirtualIP(id, true)
	v6 := DeriveVirtualIP(id, false)

	assert.NotEqual(t, v4.String(), v6.String())
	assert.Equal(t, v4, DeriveVirtualIP(id, true))
	assert.Equal(t, v6, DeriveVirtualIP(id, false))
}

func TestVirtualIPString(t *testing.T) {
	id := [NodeIDSize]byte{5, 6}
	assert.Equal(t, "10.126.5.6", VirtualIPString(id, true))

	s := VirtualIPString(id, false)
	assert.True(t, strings.HasPrefix(s, "fd00:126:126:"), "IPv6 应在预期前缀下")
}

func TestNodeIDStringRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	s := NodeIDString(kp.NodeID())
	parsed, err := NodeIDFromHex(s)
	require.NoError(t, err)
	assert.Equal(t, kp.NodeID(), parsed)
}

func TestVirtualIPv4ReservedOctetMappingIsExact(t *testing.T) {
	// 精确定义派生映射:
	//   fourth ∈ {0, 1, 255} → 2        (三个保留位统一环绕到 2)
	//   fourth = 2           → 2        (本身合法,自然保留)
	//   其它                  → 原值
	//
	// 因此 2 会出现四次(来自 0/1/2/255),其余值各出现一次。
	// 唯一"非单射"的来源就是这三个保留位,不应有其它碰撞。
	const third = 77
	counts := make(map[byte]int)

	for fourth := 0; fourth < 256; fourth++ {
		id := [NodeIDSize]byte{third, byte(fourth)}
		counts[DeriveVirtualIP(id, true).As4()[3]]++
	}

	// 逐一核对映射后的取值集合恰好是 [2, 254]。
	assert.Len(t, counts, 253, "派生结果应恰好覆盖 2..254 共 253 个值")
	for octet := 2; octet <= 254; octet++ {
		assert.Contains(t, counts, byte(octet), "缺少取值 %d", octet)
	}

	// 唯一计数为 4 的是 2;其余全部为 1。
	assert.Equal(t, 4, counts[2], "2 应由 {0,1,2,255} 四个来源合并而来")
	for octet, n := range counts {
		if octet == 2 {
			continue
		}
		assert.Equal(t, 1, n, "取值 %d 不应有多个来源", octet)
	}

	// 保留值绝不出现在结果中。
	assert.NotContains(t, counts, byte(0))
	assert.NotContains(t, counts, byte(1))
	assert.NotContains(t, counts, byte(255))
}

func TestDeriveVirtualIPv4IsInjectiveOutsideReserved(t *testing.T) {
	// 对固定第三字节,除去保留位映射,派生必须保持单射。
	const third = 200
	seen := make(map[byte]byte)

	for fourth := 2; fourth < 255; fourth++ {
		if fourth == 1 {
			continue
		}
		id := [NodeIDSize]byte{third, byte(fourth)}
		got := DeriveVirtualIP(id, true).As4()[3]

		if prev, ok := seen[got]; ok {
			t.Fatalf("非预留取值发生碰撞:%d 与 %d 都派生出 %d", prev, fourth, got)
		}
		seen[got] = byte(fourth)
	}
	assert.Len(t, seen, 253, "2..254 应全部被命中且互不重叠")
}
