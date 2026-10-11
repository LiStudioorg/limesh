// Package route 实现 Limesh 的路由层(L5):链路状态数据库、LSA 泛洪、
// Dijkstra 最短路径计算与路由表。
//
// 本包采用 OSPF 的链路状态思想,但只实现组网必需的子集(AGENTS.md 8.8):
//
//	保留:链路状态数据库 + LSA 泛洪、序号与 Age 判定新旧、Dijkstra、
//	      链路开销度量(延迟 EWMA)
//	省略:区域划分、DR/BDR 选举、完整 TLV 编码、认证字段(由 Noise 承担)
//
// 包依赖:`core/route` 不 import `core/node` 或 `core/transport`,只依赖
// `core/crypto`(NodeID 常量)与 `core/packet`(探测包编解码)。传输与发送
// 由上层通过接口驱动,使路由计算可以完全脱离网络被测试。
package route

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/LiStudioorg/limesh/core/crypto"
)

// LSA 相关常量。
const (
	// NodeIDSize 是节点标识长度,与 crypto.NodeIDSize 一致。
	NodeIDSize = crypto.NodeIDSize
	// NeighborCostSize 是单个邻居条目的线格式长度:NodeID(16) + Cost(8)。
	NeighborCostSize = NodeIDSize + 8
	// LSAHeaderSize 是 LSA 固定头长度:Origin(16)+SeqNo(4)+Age(2)+Flags(2)+Count(2)。
	LSAHeaderSize = NodeIDSize + 4 + 2 + 2 + 2

	// HopCost 是每跳的基础开销,叠加到 RTT 之上(AGENTS.md 8.6 的 cost 公式)。
	HopCost = 10
)

// LSA 相关错误。
var (
	// ErrShortLSA 表示 LSA 字节流长度不足。
	ErrShortLSA = errors.New("route: lsa buffer too short")
	// ErrBadNeighborCount 表示邻居数量与实际字节数不符。
	ErrBadNeighborCount = errors.New("route: lsa neighbor count mismatch")
	// ErrTooManyNeighbors 表示邻居数量超过编码上限。
	ErrTooManyNeighbors = errors.New("route: too many neighbors")
)

// Neighbor 描述 LSA 中的一条邻接链路。
type Neighbor struct {
	// NodeID 是邻居节点身份。
	NodeID [NodeIDSize]byte
	// Cost 是到该邻居的链路开销。
	Cost uint32
}

// LSA 是一条链路状态通告,描述发起者到其直连邻居的链路开销。
//
// 线格式(AGENTS.md 8.6.2,网络字节序大端):
//
//	Origin(16) + SeqNo(4) + Age(2) + Flags(2) + NeighborCount(2) + Neighbor[]
//	每个 Neighbor:NodeID(16) + Cost(8)
//
// 注:Cost 线格式占 8 字节但逻辑上是 uint32,高 4 字节保留为 0,为将来
// 扩大度量空间(如按微秒计的延迟)预留。
type LSA struct {
	// Origin 是发起该 LSA 的节点身份。
	Origin [NodeIDSize]byte
	// SeqNo 是 LSA 序列号,单调递增,用于判定新旧。
	SeqNo uint32
	// Age 是存活时间(秒),超时失效。
	Age uint16
	// Flags 是位标志,当前保留为 0。
	Flags uint16
	// Neighbors 是直连邻居及其链路开销。
	Neighbors []Neighbor
}

// NewLSA 构造一条空的 LSA。
func NewLSA(origin [NodeIDSize]byte) *LSA {
	return &LSA{Origin: origin, Neighbors: []Neighbor{}}
}

// AddNeighbor 追加一条邻接链路。
func (l *LSA) AddNeighbor(id [NodeIDSize]byte, cost uint32) {
	l.Neighbors = append(l.Neighbors, Neighbor{NodeID: id, Cost: cost})
}

// Cost 计算一条链路的度量:hop_cost + RTT_ms。
//
// Phase 1c 只叠加固定跳开销与 RTT;丢包惩罚留出接口(见 Weight 策略),
// 待 Phase 5 的 weighted 策略启用。
func Cost(rttMicros uint32) uint32 {
	// RTT 以毫秒计入,至少 1 ms,避免极低延迟链路把成本压到 0。
	ms := rttMicros / 1000
	if ms == 0 {
		ms = 1
	}
	return HopCost + ms
}

// MarshalBinary 把 LSA 编码为线格式。
func (l *LSA) MarshalBinary() ([]byte, error) {
	if l == nil {
		return nil, fmt.Errorf("route: marshal lsa: nil")
	}
	if len(l.Neighbors) > 0xFFFF {
		return nil, fmt.Errorf("route: marshal lsa: %d neighbors: %w", len(l.Neighbors), ErrTooManyNeighbors)
	}

	buf := make([]byte, LSAHeaderSize+len(l.Neighbors)*NeighborCostSize)

	copy(buf[0:16], l.Origin[:])
	binary.BigEndian.PutUint32(buf[16:20], l.SeqNo)
	binary.BigEndian.PutUint16(buf[20:22], l.Age)
	binary.BigEndian.PutUint16(buf[22:24], l.Flags)
	binary.BigEndian.PutUint16(buf[24:26], uint16(len(l.Neighbors)))

	off := LSAHeaderSize
	for _, n := range l.Neighbors {
		copy(buf[off:off+16], n.NodeID[:])
		// 高 4 字节保留为 0,低 4 字节写 uint32 成本。
		binary.BigEndian.PutUint32(buf[off+16+4:off+16+8], n.Cost)
		off += NeighborCostSize
	}
	return buf, nil
}

// UnmarshalBinary 从线格式解析 LSA。data 必须完整包含声明的邻居。
func (l *LSA) UnmarshalBinary(data []byte) error {
	if l == nil {
		return fmt.Errorf("route: unmarshal lsa: nil receiver")
	}
	if len(data) < LSAHeaderSize {
		return fmt.Errorf("route: unmarshal lsa: got %d bytes: %w (min %d)",
			len(data), ErrShortLSA, LSAHeaderSize)
	}

	copy(l.Origin[:], data[0:16])
	l.SeqNo = binary.BigEndian.Uint32(data[16:20])
	l.Age = binary.BigEndian.Uint16(data[20:22])
	l.Flags = binary.BigEndian.Uint16(data[22:24])
	count := int(binary.BigEndian.Uint16(data[24:26]))

	need := LSAHeaderSize + count*NeighborCostSize
	if len(data) < need {
		return fmt.Errorf("route: unmarshal lsa: %d neighbors need %d bytes, got %d: %w",
			count, need, len(data), ErrBadNeighborCount)
	}

	l.Neighbors = make([]Neighbor, count)
	off := LSAHeaderSize
	for i := 0; i < count; i++ {
		copy(l.Neighbors[i].NodeID[:], data[off:off+16])
		l.Neighbors[i].Cost = binary.BigEndian.Uint32(data[off+16+4 : off+16+8])
		off += NeighborCostSize
	}
	return nil
}

// Clone 返回 LSA 的深拷贝。
func (l *LSA) Clone() *LSA {
	if l == nil {
		return nil
	}
	cp := *l
	cp.Neighbors = append([]Neighbor(nil), l.Neighbors...)
	return &cp
}

// Equal 报告两条 LSA 的拓扑内容是否相同(忽略 Age)。
//
// Age 不参与比较:同一条 LSA 老化后内容仍然等价,不应触发重算。
func (l *LSA) Equal(other *LSA) bool {
	if l == nil || other == nil {
		return l == other
	}
	if l.Origin != other.Origin || l.SeqNo != other.SeqNo || l.Flags != other.Flags {
		return false
	}
	if len(l.Neighbors) != len(other.Neighbors) {
		return false
	}
	for i := range l.Neighbors {
		if l.Neighbors[i] != other.Neighbors[i] {
			return false
		}
	}
	return true
}

// NewerThan 报告本 LSA 是否比 other 新。
//
// 采用 OSPF 的序号比较语义:数值更大者为新;序号回绕(环绕)时,较大的
// 有符号差视为更新。Phase 1c 只做简单比较,回绕处理留作接口。
func (l *LSA) NewerThan(other *LSA) bool {
	if other == nil {
		return true
	}
	if l.SeqNo == other.SeqNo {
		return false
	}
	// 使用有符号差处理 32 位序号回绕:
	// 若 seq - other 在 (0, 2^31) 内,视为更新。
	return int32(l.SeqNo-other.SeqNo) > 0
}
