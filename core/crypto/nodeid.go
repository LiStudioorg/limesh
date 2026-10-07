package crypto

import (
	"encoding/hex"
	"fmt"
	"net/netip"
)

// 虚拟地址派生常量。
//
// 依据 AGENTS.md 8.2:
//   - VirtualIP 由 NodeID 确定性派生,任何节点都能独立算出对端的虚拟 IP,
//     不需要 DHCP,也不存在分配冲突;
//   - 派生需避开网络地址与广播地址,并跳过 10.126.126.1 保留位。
const (
	// SubnetPrefix 是默认虚拟网段的前两字节(10.126)。
	SubnetPrefix = 0x0A7E
	// SubnetHostBase 是默认网段中承载主机地址的第三字节起点(126)。
	SubnetHostBase = 126

	// ReservedGatewayOctet 是 10.126.126.1 的第四字节,保留给控制面/网关。
	ReservedGatewayOctet = 1
	// NetworkOctet 是网段地址的第四字节(10.126.126.0),不可分配给节点。
	NetworkOctet = 0
	// BroadcastOctet 是广播地址的第四字节(10.126.126.255),不可分配给节点。
	BroadcastOctet = 255

	// ipv6Prefix 是 IPv6 虚拟网段前缀 fd00:126:126::/64。
	ipv6Prefix = "fd00:126:126::/64"
)

// virtualIPv6Prefix 是解析后的 IPv6 前缀,包级初始化时确定。
var virtualIPv6Prefix = netip.MustParsePrefix(ipv6Prefix)

// DeriveVirtualIP 由 NodeID 派生虚拟 IP。
//
// ipv4 为 true 时返回 IPv4 地址:
//
//	10.126.<nodeID[0]>.<nodeID[1]>
//
// 并保证结果落在 [10.126.0.0/16] 内、避开网络地址与广播地址,以及跳过
// 10.126.126.1 保留位(AGENTS.md 8.2)。
//
// ipv4 为 false 时返回 IPv6 地址:
//
//	fd00:126:126::/64 前缀 + nodeID[0:8] 作为接口标识
//
// 派生是**纯函数**:同一 NodeID 在任何节点上、任何时刻都得到同一地址。
// 这是无中心协调的前提。
func DeriveVirtualIP(nodeID [NodeIDSize]byte, ipv4 bool) netip.Addr {
	if !ipv4 {
		return deriveIPv6(nodeID)
	}

	// 第三、四字节直接取自 NodeID 的前两字节。
	third := nodeID[0]
	fourth := nodeID[1]

	// 跳过保留位:网络地址(.0)、广播地址(.255)、网关保留(.1)。
	//
	// 直接 +1 环绕而非重哈希:重哈希会让派生结果依赖哈希函数实现细节,
	// 而简单的环绕偏移是确定且可复算的。0/255 环绕到 2,1 偏移到 2,
	// 保证结果始终是合法的主机地址。
	switch {
	case fourth == NetworkOctet || fourth == BroadcastOctet || fourth == ReservedGatewayOctet:
		fourth = 2
	}

	return netip.AddrFrom4([4]byte{byte(SubnetPrefix >> 8), byte(SubnetPrefix & 0xFF), third, fourth})
}

// deriveIPv6 由 NodeID 前 8 字节构造 IPv6 虚拟地址。
func deriveIPv6(nodeID [NodeIDSize]byte) netip.Addr {
	var raw [16]byte

	// 前缀 fd00:126:126:: 占前 6 字节,剩余 2 字节为 0。
	prefix := virtualIPv6Prefix.Addr().As16()
	copy(raw[:8], prefix[:8])

	// 接口标识取 NodeID 前 8 字节。
	copy(raw[8:], nodeID[:8])

	// 规避 ::/128 与接口标识全零的情形:全零标识会派生出前缀本身,
	// 那是网段地址而非可分配地址。
	if isZero(raw[8:]) {
		raw[15] = 1
	}

	return netip.AddrFrom16(raw)
}

// NodeIDFromHex 解析十六进制 NodeID。
//
// 用于从配置或命令行读取对端身份。长度不符即报错,不做静默截断或补齐。
func NodeIDFromHex(s string) ([NodeIDSize]byte, error) {
	var id [NodeIDSize]byte

	raw, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("crypto: parse node id %q: %w", s, err)
	}
	if len(raw) != NodeIDSize {
		return id, fmt.Errorf("crypto: parse node id %q: got %d bytes: %w (want %d)",
			s, len(raw), ErrKeyFileSize, NodeIDSize)
	}

	copy(id[:], raw)
	return id, nil
}

// PublicKeyFromHex 解析十六进制公钥。
func PublicKeyFromHex(s string) ([KeySize]byte, error) {
	var pub [KeySize]byte

	raw, err := hex.DecodeString(s)
	if err != nil {
		return pub, fmt.Errorf("crypto: parse public key %q: %w", s, err)
	}
	if len(raw) != KeySize {
		return pub, fmt.Errorf("crypto: parse public key %q: got %d bytes: %w (want %d)",
			s, len(raw), ErrKeyFileSize, KeySize)
	}

	copy(pub[:], raw)
	return pub, nil
}

// NodeIDString 返回 NodeID 的十六进制表示。
func NodeIDString(id [NodeIDSize]byte) string {
	return hex.EncodeToString(id[:])
}

// VirtualIPString 返回派生虚拟 IP 的字符串形式,用于日志与拓扑展示。
func VirtualIPString(nodeID [NodeIDSize]byte, ipv4 bool) string {
	return DeriveVirtualIP(nodeID, ipv4).String()
}
