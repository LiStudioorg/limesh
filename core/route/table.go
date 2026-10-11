package route

import (
	"container/heap"
	"sort"
	"sync"
)

// RouteEntry 是一条路由表项。
type RouteEntry struct {
	// Dst 是目的节点身份。
	Dst [NodeIDSize]byte
	// NextHop 是下一跳的直接邻居 NodeID。
	//
	// 当 Dst 本身是直连邻居时,NextHop == Dst。
	NextHop [NodeIDSize]byte
	// Cost 是到目的节点的累计路径开销。
	Cost uint32
	// Hops 是到目的节点的跳数。
	Hops uint8
}

// RouteTable 是到全网各节点的转发表。
//
// 并发安全:Lookup/Set/Dump 可并发调用。
type RouteTable struct {
	mu      sync.RWMutex
	entries map[[NodeIDSize]byte]RouteEntry
}

// NewRouteTable 创建一个空路由表。
func NewRouteTable() *RouteTable {
	return &RouteTable{entries: make(map[[NodeIDSize]byte]RouteEntry)}
}

// Lookup 查询到目的节点的路由。
func (t *RouteTable) Lookup(dst [NodeIDSize]byte) (RouteEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[dst]
	return e, ok
}

// Set 原子替换整张路由表。
//
// 用整表替换而非逐条更新,保证查表者永远看到一致的快照,不会读到
// "算了一半"的中间态。
func (t *RouteTable) Set(entries []RouteEntry) {
	m := make(map[[NodeIDSize]byte]RouteEntry, len(entries))
	for _, e := range entries {
		m[e.Dst] = e
	}

	t.mu.Lock()
	t.entries = m
	t.mu.Unlock()
}

// Dump 返回路由表全部条目,按目的 NodeID 字节序排序,保证输出稳定。
func (t *RouteTable) Dump() []RouteEntry {
	t.mu.RLock()
	out := make([]RouteEntry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	t.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		for k := 0; k < NodeIDSize; k++ {
			if out[i].Dst[k] != out[j].Dst[k] {
				return out[i].Dst[k] < out[j].Dst[k]
			}
		}
		return false
	})
	return out
}

// Len 返回路由表条目数。
func (t *RouteTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// ---------------------------------------------------------------------------
// Dijkstra
// ---------------------------------------------------------------------------

// edge 是拓扑图中的一条有向边。
type edge struct {
	to   [NodeIDSize]byte
	cost uint32
}

// Compute 从拓扑库计算以 self 为根的最短路径树,生成路由表。
//
// 使用标准二叉堆(container/heap)实现 Dijkstra。图由 LSA 集合隐式定义:
// 每条无向链路在双方的 LSA 中各出现一次(若只有一方声明,则该链路仅单向可用,
// 这与 OSPF 的实际行为一致——双方必须互认才形成可用邻接)。
//
// 说明:
//   - self 会出现在结果中(Dst==self, Cost==0, Hops==0),便于调用方识别自身;
//   - 不可达节点不进入结果;
//   - 直连邻居的 NextHop 即该邻居自身。
func Compute(db *TopologyDB, self [NodeIDSize]byte, strategy Strategy) *RouteTable {
	if strategy == nil {
		strategy = shortestStrategy{}
	}

	lsas := db.Snapshot()

	// 构建邻接表。每个节点从自己的 LSA 展开出到各邻居的边。
	// 只有在**双方都声明**该链路时才视为可用(双向可达)。
	declared := make(map[[NodeIDSize]byte]map[[NodeIDSize]byte]uint32)
	for _, l := range lsas {
		m := declared[l.Origin]
		if m == nil {
			m = make(map[[NodeIDSize]byte]uint32)
			declared[l.Origin] = m
		}
		for _, n := range l.Neighbors {
			m[n.NodeID] = n.Cost
		}
	}

	adj := make(map[[NodeIDSize]byte][]edge)
	for origin, neighbors := range declared {
		for nb, cost := range neighbors {
			// 双向可达:邻居也必须声明回 origin。
			back, ok := declared[nb]
			if !ok {
				continue
			}
			if _, ok := back[origin]; !ok {
				continue
			}
			adj[origin] = append(adj[origin], edge{to: nb, cost: cost})
		}
	}

	// Dijkstra。
	dist := map[[NodeIDSize]byte]uint32{self: 0}
	hops := map[[NodeIDSize]byte]uint8{self: 0}
	nextHop := map[[NodeIDSize]byte][NodeIDSize]byte{}
	visited := map[[NodeIDSize]byte]bool{}

	pq := &nodeHeap{}
	heap.Init(pq)
	heap.Push(pq, heapItem{node: self, cost: 0})

	for pq.Len() > 0 {
		item := heap.Pop(pq).(heapItem)
		u := item.node
		if visited[u] {
			continue
		}
		visited[u] = true

		for _, e := range adj[u] {
			if visited[e.to] {
				continue
			}
			// 本链路的开销加上从源点到 u 的跳数影响由策略决定。
			w := strategy.Weight(hops[u], e.cost, 0)
			nd := dist[u] + w
			cur, seen := dist[e.to]
			if !seen || nd < cur {
				dist[e.to] = nd
				hops[e.to] = hops[u] + 1
				// 下一跳:若 u 就是 self,则下一跳是 e.to;否则继承 u 的下一跳。
				if u == self {
					nextHop[e.to] = e.to
				} else {
					nextHop[e.to] = nextHop[u]
				}
				heap.Push(pq, heapItem{node: e.to, cost: nd})
			}
		}
	}

	entries := make([]RouteEntry, 0, len(dist))
	for dst, cost := range dist {
		entries = append(entries, RouteEntry{
			Dst:     dst,
			NextHop: nextHop[dst], // self 的 NextHop 为零值
			Cost:    cost,
			Hops:    hops[dst],
		})
	}

	rt := NewRouteTable()
	rt.Set(entries)
	return rt
}

// heapItem 是 Dijkstra 优先队列的元素。
type heapItem struct {
	node [NodeIDSize]byte
	cost uint32
}

// nodeHeap 是按 cost 排序的最小堆。
type nodeHeap []heapItem

func (h nodeHeap) Len() int           { return len(h) }
func (h nodeHeap) Less(i, j int) bool { return h[i].cost < h[j].cost }
func (h nodeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x any)        { *h = append(*h, x.(heapItem)) }
func (h *nodeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}
