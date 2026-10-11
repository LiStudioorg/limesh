package route

import (
	"sync"
)

// TopologyDB 是链路状态数据库,保存全网的 LSA。
//
// 并发安全:Update/Get/Snapshot/Age 均可由多个 goroutine 并发调用。
// 每次拓扑内容发生实质性变化时 version 递增,上层据此触发路由重算,
// 避免每次收到同一条 LSA 都重算。
type TopologyDB struct {
	mu   sync.RWMutex
	lsas map[[NodeIDSize]byte]*LSA
	// version 是拓扑版本号,内容变化时递增。
	version uint64
	// maxAge 是 LSA 的失效秒数;超过则被 Age 删除。
	maxAge uint16
}

// NewTopologyDB 创建一个空拓扑库。maxAge 为 0 时使用 LSAMaxAge 默认值。
func NewTopologyDB(maxAge uint16) *TopologyDB {
	if maxAge == 0 {
		maxAge = LSAMaxAge
	}
	return &TopologyDB{
		lsas:   make(map[[NodeIDSize]byte]*LSA),
		maxAge: maxAge,
	}
}

// Update 合并一条 LSA,返回拓扑内容是否发生实质性变化。
//
// 序号规则(避免环路的关键,AGENTS.md 8.6.2):
//   - 无同源 LSA:接受;
//   - new.SeqNo > old.SeqNo:接受并覆盖;
//   - new.SeqNo == old.SeqNo:丢弃(除非来自本节点的直接刷新);
//   - new.SeqNo < old.SeqNo:丢弃(旧信息);
//
// 注意:收到任何同源 LSA 都会刷新 Age(重置老化计时),即便序号相同
// ——这表示该源头仍然存活。
func (db *TopologyDB) Update(lsa *LSA) bool {
	if lsa == nil {
		return false
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	old, exists := db.lsas[lsa.Origin]

	// 刷新 Age 计时:同源 LSA 到达即视为源头存活。
	refresh := lsa.Clone()
	refresh.Age = 0

	if !exists {
		db.lsas[lsa.Origin] = refresh
		db.version++
		return true
	}

	// 序号较新:覆盖。
	if lsa.NewerThan(old) {
		changed := !refresh.Equal(old)
		db.lsas[lsa.Origin] = refresh
		if changed {
			db.version++
		}
		return changed
	}

	// 序号相同:内容与序号都一致,仅刷新 Age 计时(源头仍存活),
	// 不改变拓扑版本。内容不同的"同序号"属于异常(序号应随发布递增),
	// 保守地不接受其内容。
	if lsa.SeqNo == old.SeqNo {
		old.Age = 0
		return false
	}

	// 更旧:丢弃。
	return false
}

// Refresh 刷新指定源的 Age(收到同源 LSA 但无需更新内容时调用)。
func (db *TopologyDB) Refresh(origin [NodeIDSize]byte) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if l, ok := db.lsas[origin]; ok {
		l.Age = 0
	}
}

// Get 取回指定源的 LSA 副本。
func (db *TopologyDB) Get(origin [NodeIDSize]byte) (*LSA, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	l, ok := db.lsas[origin]
	if !ok {
		return nil, false
	}
	return l.Clone(), true
}

// Snapshot 返回全部 LSA 的深拷贝列表,供路由计算使用。
//
// 计算在副本上进行,避免长时间持有锁,也避免计算期间拓扑被修改。
func (db *TopologyDB) Snapshot() []*LSA {
	db.mu.RLock()
	defer db.mu.RUnlock()

	out := make([]*LSA, 0, len(db.lsas))
	for _, l := range db.lsas {
		out = append(out, l.Clone())
	}
	return out
}

// Len 返回拓扑库中的 LSA 数量。
func (db *TopologyDB) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.lsas)
}

// Version 返回当前拓扑版本号。
func (db *TopologyDB) Version() uint64 {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.version
}

// Age 把所有 LSA 的 Age 增加 seconds,并删除超过 maxAge 的条目。
//
// 返回被删除的 LSA 数量(>0 表示拓扑发生变化,上层应触发重算)。
// 老化是发现"节点静默消失"的手段:即便没有显式的删除消息,长期不刷新
// 的 LSA 也会因超龄被清除(AGENTS.md 8.6.2)。
func (db *TopologyDB) Age(seconds uint16) int {
	if seconds == 0 {
		return 0
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	var removed int
	for origin, l := range db.lsas {
		// 防止 uint16 溢出:接近上限时直接封顶,由后续判断删除。
		if int(l.Age)+int(seconds) >= int(db.maxAge) {
			delete(db.lsas, origin)
			removed++
			continue
		}
		l.Age += seconds
	}
	if removed > 0 {
		db.version++
	}
	return removed
}

// Remove 删除指定源的 LSA(如对端显式下线)。
//
// 返回是否确实删除了条目。
func (db *TopologyDB) Remove(origin [NodeIDSize]byte) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	if _, ok := db.lsas[origin]; !ok {
		return false
	}
	delete(db.lsas, origin)
	db.version++
	return true
}
