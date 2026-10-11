package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTopologyUpdateAndGet 验证更新与查询。
func TestTopologyUpdateAndGet(t *testing.T) {
	db := NewTopologyDB(0)

	l := NewLSA(nid(1))
	l.SeqNo = 1
	l.AddNeighbor(nid(2), 5)

	assert.True(t, db.Update(l), "首次更新应报告变化")
	assert.Equal(t, 1, db.Len())

	got, ok := db.Get(nid(1))
	require.True(t, ok)
	assert.Equal(t, uint32(1), got.SeqNo)
	assert.Equal(t, uint16(0), got.Age)

	_, ok = db.Get(nid(99))
	assert.False(t, ok)
}

// TestTopologyVersionIncrements 验证版本号递增。
func TestTopologyVersionIncrements(t *testing.T) {
	db := NewTopologyDB(0)
	v0 := db.Version()

	l := NewLSA(nid(1))
	l.SeqNo = 1
	db.Update(l)
	assert.Greater(t, db.Version(), v0)
}

// TestTopologyOlderLSADropped 验证旧序号不覆盖新序号。
func TestTopologyOlderLSADropped(t *testing.T) {
	db := NewTopologyDB(0)

	newer := NewLSA(nid(1))
	newer.SeqNo = 5
	newer.AddNeighbor(nid(2), 5)
	require.True(t, db.Update(newer))

	older := NewLSA(nid(1))
	older.SeqNo = 3
	older.AddNeighbor(nid(3), 9)
	assert.False(t, db.Update(older), "旧序号应被丢弃")

	got, _ := db.Get(nid(1))
	assert.Equal(t, uint32(5), got.SeqNo, "应保留较新的 LSA")
	require.Len(t, got.Neighbors, 1)
	assert.Equal(t, nid(2), got.Neighbors[0].NodeID)
}

// TestTopologySameSeqNoRefreshesAge 验证相同序号刷新 Age 但不改版本。
func TestTopologySameSeqNoRefreshesAge(t *testing.T) {
	db := NewTopologyDB(0)

	l := NewLSA(nid(1))
	l.SeqNo = 5
	l.Age = 100
	db.Update(l)

	// 老化一下,Age 上升。
	db.Age(50)
	got, _ := db.Get(nid(1))
	assert.Equal(t, uint16(50), got.Age)

	// 收到同序号 LSA:刷新 Age 回 0,不改变版本。
	vBefore := db.Version()
	same := NewLSA(nid(1))
	same.SeqNo = 5
	db.Update(same)
	assert.Equal(t, vBefore, db.Version(), "同序号不应改变拓扑版本")

	got, _ = db.Get(nid(1))
	assert.Equal(t, uint16(0), got.Age, "同序号应刷新 Age")
}

// TestTopologyNewSeqChangesContent 验证新序号且内容变化触发版本递增。
func TestTopologyNewSeqChangesContent(t *testing.T) {
	db := NewTopologyDB(0)

	l1 := NewLSA(nid(1))
	l1.SeqNo = 1
	l1.AddNeighbor(nid(2), 5)
	db.Update(l1)

	vBefore := db.Version()
	l2 := NewLSA(nid(1))
	l2.SeqNo = 2
	l2.AddNeighbor(nid(2), 5)
	l2.AddNeighbor(nid(3), 7)
	assert.True(t, db.Update(l2), "内容变化应报告 true")
	assert.Greater(t, db.Version(), vBefore)
}

// TestTopologyAge 覆盖老化与超龄删除。
func TestTopologyAge(t *testing.T) {
	db := NewTopologyDB(300)

	l := NewLSA(nid(1))
	l.SeqNo = 1
	db.Update(l)

	// 老化 100 秒,未超龄。
	assert.Equal(t, 0, db.Age(100))
	got, _ := db.Get(nid(1))
	assert.Equal(t, uint16(100), got.Age)

	// 再老化 200 秒,达到 300 上限,应被删除。
	assert.Equal(t, 1, db.Age(200))
	assert.Equal(t, 0, db.Len())

	// Age(0) 是无操作。
	assert.Equal(t, 0, db.Age(0))
}

// TestTopologySnapshotIsCopy 验证 Snapshot 返回深拷贝。
func TestTopologySnapshotIsCopy(t *testing.T) {
	db := NewTopologyDB(0)
	l := NewLSA(nid(1))
	l.SeqNo = 1
	l.AddNeighbor(nid(2), 5)
	db.Update(l)

	snap := db.Snapshot()
	require.Len(t, snap, 1)
	snap[0].Neighbors[0].Cost = 999

	got, _ := db.Get(nid(1))
	assert.Equal(t, uint32(5), got.Neighbors[0].Cost, "快照修改不应影响库")
}

// TestTopologyRemove 验证显式删除。
func TestTopologyRemove(t *testing.T) {
	db := NewTopologyDB(0)
	l := NewLSA(nid(1))
	l.SeqNo = 1
	db.Update(l)

	vBefore := db.Version()
	assert.True(t, db.Remove(nid(1)))
	assert.Equal(t, 0, db.Len())
	assert.Greater(t, db.Version(), vBefore)

	assert.False(t, db.Remove(nid(1)), "重复删除返回 false")
}

// TestTopologyRefresh 验证主动刷新 Age。
func TestTopologyRefresh(t *testing.T) {
	db := NewTopologyDB(0)
	l := NewLSA(nid(1))
	l.SeqNo = 1
	db.Update(l)
	db.Age(50)

	db.Refresh(nid(1))
	got, _ := db.Get(nid(1))
	assert.Equal(t, uint16(0), got.Age)
}
