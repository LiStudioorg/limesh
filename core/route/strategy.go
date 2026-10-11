package route

import (
	"errors"
	"fmt"
)

// 默认 LSA 失效秒数,与 AGENTS.md 8.11 的 LSAMaxAge 一致。
const LSAMaxAge = 300

// 路由策略错误。
var (
	// ErrUnknownStrategy 表示请求的路由策略不存在。
	ErrUnknownStrategy = errors.New("route: unknown strategy")
)

// Strategy 是路由开销计算策略。
//
// 策略只影响**如何把一条链路的度量折算为 Dijkstra 的边权**,不参与最短路
// 计算本身。这样新增策略无需改动路由计算。
//
// 边权语义的约定:
//   - shortest 忽略链路度量,每条边恒为 HopCost,于是 Dijkstra 退化为
//     "跳数最少";
//   - latency 直接采用链路度量(已由 LSA 的 Cost 字段按 RTT 折算),
//     于是 Dijkstra 选累计延迟最低;
//   - weighted 使用链路度量(Phase 5 再叠加手工权重表)。
type Strategy interface {
	// Name 返回策略注册名,与配置 routing.strategy 一致。
	Name() string
	// Weight 把一条链路的度量折算为 Dijkstra 边权。
	//
	// hops 是从源点出发已过的跳数;edgeCost 是该链路的原始度量
	// (由 route.Cost 按 RTT 折算),lossRate 是丢包率 [0,1]。
	Weight(hops uint8, edgeCost uint32, lossRate float64) uint32
}

// shortestStrategy 只按跳数计费,忽略链路质量。
//
// 适用于结构稳定的固定拓扑:路径不随瞬时抖动迁移。
type shortestStrategy struct{}

// Name 返回策略名。
func (shortestStrategy) Name() string { return "shortest" }

// Weight 返回固定跳开销,使累计成本正比于跳数。
func (shortestStrategy) Weight(hops uint8, edgeCost uint32, lossRate float64) uint32 {
	return HopCost
}

// latencyStrategy 以链路度量(平滑 RTT)为边权,叠加丢包惩罚。
//
// Phase 1c 已完成 RTT 部分;丢包惩罚的权重留待 Phase 5 的 weighted 策略细化。
type latencyStrategy struct{}

// Name 返回策略名。
func (latencyStrategy) Name() string { return "latency" }

// Weight 返回 edgeCost + loss×1000(AGENTS.md 8.6 的 cost 公式)。
func (latencyStrategy) Weight(hops uint8, edgeCost uint32, lossRate float64) uint32 {
	return edgeCost + uint32(lossRate*1000)
}

// weightedStrategy 按管理员权重选路。
type weightedStrategy struct{}

// Name 返回策略名。
func (weightedStrategy) Name() string { return "weighted" }

// Weight 返回链路度量。
//
// Phase 1c 只保留接口与最基本语义;完整的权重表解析与多路径分流属 Phase 5。
func (weightedStrategy) Weight(hops uint8, edgeCost uint32, lossRate float64) uint32 {
	return edgeCost
}

// NewStrategy 按名称构造策略。
//
// 支持 shortest / latency / weighted。weighted 目前与 shortest 等价
// (权重表在 Phase 5 接入),但注册名可用,避免配置校验失败。
func NewStrategy(name string) (Strategy, error) {
	switch name {
	case "", "shortest":
		return shortestStrategy{}, nil
	case "latency":
		return latencyStrategy{}, nil
	case "weighted":
		return weightedStrategy{}, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, name)
	}
}
