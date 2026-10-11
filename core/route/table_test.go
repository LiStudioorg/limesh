package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildLinear 构建 A-B-C 直线:双向链路。
func buildLinear(db *TopologyDB, a, b, c [NodeIDSize]byte, cost uint32) {
	la := NewLSA(a)
	la.SeqNo = 1
	la.AddNeighbor(b, cost)
	db.Update(la)

	lb := NewLSA(b)
	lb.SeqNo = 1
	lb.AddNeighbor(a, cost)
	lb.AddNeighbor(c, cost)
	db.Update(lb)

	lc := NewLSA(c)
	lc.SeqNo = 1
	lc.AddNeighbor(b, cost)
	db.Update(lc)
}

// TestComputeLinear 验证直线拓扑 A-B-C 的最短路径与下一跳。
func TestComputeLinear(t *testing.T) {
	db := NewTopologyDB(0)
	a, b, c := nid(1), nid(2), nid(3)
	buildLinear(db, a, b, c, 10)

	rt := Compute(db, a, nil)

	// 自己:cost 0, hops 0。
	self, ok := rt.Lookup(a)
	require.True(t, ok)
	assert.Equal(t, uint32(0), self.Cost)
	assert.Equal(t, uint8(0), self.Hops)

	// 到 B:直连,nextHop = B。
	toB, ok := rt.Lookup(b)
	require.True(t, ok)
	assert.Equal(t, b, toB.NextHop)
	assert.Equal(t, uint8(1), toB.Hops)

	// 到 C:经 B 中转,nextHop = B。
	toC, ok := rt.Lookup(c)
	require.True(t, ok)
	assert.Equal(t, b, toC.NextHop, "到 C 的下一跳应为 B")
	assert.Equal(t, uint8(2), toC.Hops)
	assert.Greater(t, toC.Cost, toB.Cost)
}

// TestComputeDiamond 验证菱形拓扑选择开销更小的路径。
func TestComputeDiamond(t *testing.T) {
	db := NewTopologyDB(0)
	a, b, c, d := nid(1), nid(2), nid(3), nid(4)

	// A-B-D 与 A-C-D 两条路径,B 侧更便宜。
	set := func(origin [NodeIDSize]byte, neighbors map[[NodeIDSize]byte]uint32) {
		l := NewLSA(origin)
		l.SeqNo = 1
		for nb, cost := range neighbors {
			l.AddNeighbor(nb, cost)
		}
		db.Update(l)
	}

	set(a, map[[NodeIDSize]byte]uint32{b: 10, c: 10})
	set(b, map[[NodeIDSize]byte]uint32{a: 10, d: 5})
	set(c, map[[NodeIDSize]byte]uint32{a: 10, d: 50})
	set(d, map[[NodeIDSize]byte]uint32{b: 5, c: 50})

	// 用 latency 策略让链路度量参与选路;shortest 只比跳数,两条路径同为
	// 2 跳,无法区分。
	lat, err := NewStrategy("latency")
	require.NoError(t, err)

	rt := Compute(db, a, lat)
	toD, ok := rt.Lookup(d)
	require.True(t, ok)
	assert.Equal(t, b, toD.NextHop, "应选择经 B 的更低开销路径")
}

// TestComputeUnreachable 验证不可达节点不进入路由表。
func TestComputeUnreachable(t *testing.T) {
	db := NewTopologyDB(0)
	a, b, isolated := nid(1), nid(2), nid(9)

	la := NewLSA(a)
	la.SeqNo = 1
	la.AddNeighbor(b, 10)
	db.Update(la)
	lb := NewLSA(b)
	lb.SeqNo = 1
	lb.AddNeighbor(a, 10)
	db.Update(lb)

	// isolated 有自己的 LSA 但无人与它相连。
	li := NewLSA(isolated)
	li.SeqNo = 1
	db.Update(li)

	rt := Compute(db, a, nil)
	_, ok := rt.Lookup(b)
	assert.True(t, ok)
	_, ok = rt.Lookup(isolated)
	assert.False(t, ok, "不可达节点不应出现在路由表")
}

// TestComputeUnidirectionalLinkIgnored 验证单向声明不构成可用链路。
func TestComputeUnidirectionalLinkIgnored(t *testing.T) {
	db := NewTopologyDB(0)
	a, b := nid(1), nid(2)

	// 只有 A 声明到 B,B 不声明回 A。
	la := NewLSA(a)
	la.SeqNo = 1
	la.AddNeighbor(b, 10)
	db.Update(la)

	lb := NewLSA(b)
	lb.SeqNo = 1
	db.Update(lb)

	rt := Compute(db, a, nil)
	_, ok := rt.Lookup(b)
	assert.False(t, ok, "单向链路不应形成邻接")
}

// TestComputeRing 验证环形拓扑。
func TestComputeRing(t *testing.T) {
	db := NewTopologyDB(0)
	n := [4][NodeIDSize]byte{nid(1), nid(2), nid(3), nid(4)}
	cost := uint32(10)

	for i := range n {
		l := NewLSA(n[i])
		l.SeqNo = 1
		l.AddNeighbor(n[(i+1)%4], cost)
		l.AddNeighbor(n[(i+3)%4], cost)
		db.Update(l)
	}

	rt := Compute(db, n[0], nil)
	// 到对角的 n[2] 有两条等长路径,应可达且 2 跳。
	e, ok := rt.Lookup(n[2])
	require.True(t, ok)
	assert.Equal(t, uint8(2), e.Hops)
}

// TestComputeStar 验证星形拓扑。
func TestComputeStar(t *testing.T) {
	db := NewTopologyDB(0)
	hub := nid(1)
	leaves := []byte{2, 3, 4, 5}

	hl := NewLSA(hub)
	hl.SeqNo = 1
	for _, lf := range leaves {
		hl.AddNeighbor(nid(lf), 10)
	}
	db.Update(hl)

	for _, lf := range leaves {
		ll := NewLSA(nid(lf))
		ll.SeqNo = 1
		ll.AddNeighbor(hub, 10)
		db.Update(ll)
	}

	rt := Compute(db, nid(2), nil)
	for _, lf := range leaves {
		if lf == 2 {
			continue
		}
		e, ok := rt.Lookup(nid(lf))
		require.True(t, ok, "叶子 %d 应可达", lf)
		assert.Equal(t, hub, e.NextHop, "应经 hub 中转")
		assert.Equal(t, uint8(2), e.Hops)
	}
}

// TestComputeStrategySelection 验证策略影响开销但不改变可达性。
func TestComputeStrategySelection(t *testing.T) {
	db := NewTopologyDB(0)
	a, b, c := nid(1), nid(2), nid(3)
	buildLinear(db, a, b, c, 5000)

	short, err := NewStrategy("shortest")
	require.NoError(t, err)
	lat, err := NewStrategy("latency")
	require.NoError(t, err)

	rtShort := Compute(db, a, short)
	rtLat := Compute(db, a, lat)

	eShort, _ := rtShort.Lookup(c)
	eLat, _ := rtLat.Lookup(c)
	assert.Equal(t, b, eShort.NextHop)
	assert.Equal(t, b, eLat.NextHop)
	// shortest 每跳固定 HopCost,latency 叠加 RTT 惩罚,故更大。
	assert.Less(t, eShort.Cost, eLat.Cost)
}

// TestComputeEmptyTopology 验证空拓扑只含自己。
func TestComputeEmptyTopology(t *testing.T) {
	db := NewTopologyDB(0)
	a := nid(1)

	rt := Compute(db, a, nil)
	assert.Equal(t, 1, rt.Len())
	_, ok := rt.Lookup(a)
	assert.True(t, ok)
}

// TestRouteTableSetAndDump 验证整表替换与稳定 dump。
func TestRouteTableSetAndDump(t *testing.T) {
	rt := NewRouteTable()
	rt.Set([]RouteEntry{
		{Dst: nid(3), NextHop: nid(2), Cost: 20, Hops: 2},
		{Dst: nid(1), NextHop: nid(1), Cost: 0, Hops: 0},
	})

	dump := rt.Dump()
	require.Len(t, dump, 2)
	assert.Equal(t, nid(1), dump[0].Dst, "dump 应排序")
	assert.Equal(t, nid(3), dump[1].Dst)

	// 整表替换:旧条目消失。
	rt.Set([]RouteEntry{{Dst: nid(5), NextHop: nid(5)}})
	_, ok := rt.Lookup(nid(1))
	assert.False(t, ok)
	assert.Equal(t, 1, rt.Len())
}
