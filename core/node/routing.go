package node

import (
	"context"
	"fmt"
	"net"
	"time"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/peer"
	"github.com/LiStudioorg/limesh/core/route"
)

// ---------------------------------------------------------------------------
// LSA 构建与发布
// ---------------------------------------------------------------------------

// buildSelfLSA 依据当前直接邻居构建本节点的 LSA。
//
// 邻居开销取自 Prober 采样的 RTT(经 route.Cost 折算为 hop + RTT_ms);
// 尚无采样时使用固定 HopCost。
func (d *datapath) buildSelfLSA() *route.LSA {
	d.lsaMu.Lock()
	seq := d.selfLSA.SeqNo + 1
	d.lsaMu.Unlock()

	lsa := route.NewLSA(d.localID)
	lsa.SeqNo = seq

	for _, p := range d.peers.All() {
		st := p.State()
		// 只有已建立双向可达路径的邻居才写入 LSA。
		if st != peer.StateDirect && st != peer.StateRelayed {
			continue
		}
		cost := uint32(route.HopCost)
		if d.prober != nil {
			stats := d.prober.Stats(p.NodeID)
			if stats.RTT > 0 {
				cost = route.Cost(stats.RTTMicros)
			}
		}
		lsa.AddNeighbor(p.NodeID, cost)
	}

	d.lsaMu.Lock()
	d.selfLSA = lsa
	d.lsaMu.Unlock()

	// 本节点 LSA 也要进入自己的拓扑库:Dijkstra 从 self 出发需要 self 的出边,
	// 少了它会导致本节点没有任何邻居,所有路由都退化为"只有自己"。
	d.topology.Update(lsa)
	return lsa
}

// SelfLSA 返回本节点当前 LSA 的副本。
func (d *datapath) SelfLSA() *route.LSA {
	d.lsaMu.Lock()
	defer d.lsaMu.Unlock()
	return d.selfLSA.Clone()
}

// localLSAstale 判断本节点 LSA 的序号是否落后于已发布值。
func (d *datapath) bumpLSASeqNo() {
	d.lsaMu.Lock()
	d.selfLSA.SeqNo++
	d.lsaMu.Unlock()
}

// encodeAndSendLSA 把本节点 LSA 编码后通过 send 发送给指定对端。
func (d *datapath) lsaPayload(lsa *route.LSA) ([]byte, error) {
	data, err := lsa.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("node: marshal lsa: %w", err)
	}
	return data, nil
}

// broadcastSelfLSA 把本节点 LSA 洪泛给所有活跃邻居。
func (d *datapath) broadcastSelfLSA() {
	lsa := d.buildSelfLSA()
	payload, err := d.lsaPayload(lsa)
	if err != nil {
		d.log.Warn("encode self lsa failed", zap.Error(err))
		return
	}

	sent := d.peers.Broadcast(payload, func(peerID [crypto.NodeIDSize]byte, data []byte) error {
		return d.sendControl(peerID, packet.TypeLSA, data)
	})

	// 本节点 LSA 的邻居集合可能变化(新邻居加入/离开),据此重算路由。
	d.recomputeRoutes()

	d.log.Debug("lsa broadcast", zap.Uint32("seq", lsa.SeqNo), zap.Int("neighbors", sent))
}

// ---------------------------------------------------------------------------
// LSA 接收与洪泛
// ---------------------------------------------------------------------------

// handleLSA 处理一条来自 from 的 LSA:更新拓扑、按需洪泛。
func (d *datapath) handleLSA(from [crypto.NodeIDSize]byte, payload []byte) error {
	var lsa route.LSA
	if err := lsa.UnmarshalBinary(payload); err != nil {
		return fmt.Errorf("node: unmarshal lsa from %s: %w", crypto.NodeIDString(from), err)
	}

	changed := d.topology.Update(&lsa)

	// 洪泛:凡是序号更新的 LSA 都要转发给除来源外的邻居,保证全网收敛。
	// 即便是本节点已见过的重复包(序号未更新)也不再转发,避免风暴。
	d.forwardLSA(from, &lsa)

	if changed {
		d.recomputeRoutes()
	}
	return nil
}

// forwardLSA 把 LSA 转发给除来源外的所有活跃邻居(洪泛)。
//
// 不转发回来源,避免立即回环(AGENTS.md 8.6.2)。序号机制保证环路中的
// 重复包不会无限传播:接收到已见过的序号时拓扑不变化,也不会再次转发
// (由 handleLSA 对 changed 的判断体现——此处按序号去重)。
func (d *datapath) forwardLSA(from [crypto.NodeIDSize]byte, lsa *route.LSA) {
	d.lsaMu.Lock()
	last, seen := d.seenLSA[lsa.Origin]
	if seen && !lsa.NewerThan(&route.LSA{SeqNo: last}) {
		d.lsaMu.Unlock()
		return
	}
	d.seenLSA[lsa.Origin] = lsa.SeqNo
	d.lsaMu.Unlock()

	payload, err := d.lsaPayload(lsa)
	if err != nil {
		return
	}
	d.peers.BroadcastExcept(from, payload, func(peerID [crypto.NodeIDSize]byte, data []byte) error {
		return d.sendControl(peerID, packet.TypeLSA, data)
	})
}

// sendControl 使用 peerID 的会话加密并发送一段控制载荷。
//
// 控制消息(LSA/探测)走与数据相同的会话加密与寻址路径,但不经过 IP
// 路由:它们总是在直接邻居之间一跳送达。
func (d *datapath) sendControl(peerID [crypto.NodeIDSize]byte, pktType byte, payload []byte) error {
	s, ok := d.sessions.GetByPeer(peerID)
	if !ok || s.Closed() {
		return fmt.Errorf("node: send control: no session for peer %s", crypto.NodeIDString(peerID))
	}
	addr, ok := d.peerAddr(peerID)
	if !ok {
		return fmt.Errorf("node: send control: no address for peer %s", crypto.NodeIDString(peerID))
	}

	aad := buildAAD(d.localID, peerID, s.ID)
	seq, ciphertext, err := s.SealNext(payload, aad)
	if err != nil {
		return fmt.Errorf("node: seal control: %w", err)
	}

	pkt := packet.New(ciphertext)
	pkt.SetType(pktType)
	pkt.SetFlags(packet.FlagEncrypted)
	pkt.SetSessionID(s.ID)
	pkt.SetSequence(uint32(seq))
	if err := pkt.SetSrcNodeID(d.localID[:]); err != nil {
		return err
	}
	if err := pkt.SetDstNodeID(peerID[:]); err != nil {
		return err
	}
	return d.sendRaw(pkt, addr)
}

// ---------------------------------------------------------------------------
// 路由重算
// ---------------------------------------------------------------------------

// recomputeRoutes 从拓扑库重新计算转发表并原子替换。
func (d *datapath) recomputeRoutes() {
	rt := route.Compute(d.topology, d.localID, d.strategy)
	d.routeTable.Store(rt)

	d.log.Debug("routes recomputed",
		zap.Int("entries", rt.Len()),
		zap.Uint64("topology_version", d.topology.Version()),
	)
	if d.onRouteChange != nil {
		d.onRouteChange(rt)
	}
}

// RouteTable 返回当前转发表。返回的指针在下次重算前保持有效。
func (d *datapath) RouteTable() *route.RouteTable { return d.routeTable.Load() }

// Topology 返回链路状态数据库,供上层观测。
func (d *datapath) Topology() *route.TopologyDB { return d.topology }

// RouteTo 查询到目的 NodeID 的路由。
func (d *datapath) RouteTo(dst [crypto.NodeIDSize]byte) (route.RouteEntry, bool) {
	rt := d.routeTable.Load()
	if rt == nil {
		return route.RouteEntry{}, false
	}
	return rt.Lookup(dst)
}

// ---------------------------------------------------------------------------
// 逐跳数据转发
// ---------------------------------------------------------------------------

// forwardTransit 处理一个目的不是本节点的数据包:查路由并转发给下一跳。
//
// 采用**逐跳加密**:本跳用接收会话解密得到内层明文,再用到下一跳的会话
// 重新加密发送。ZCPacket 的 Src/Dst NodeID 保持不变,因此最终接收方能
// 识别端到端来源(AGENTS.md 8.6.4 的中继语义在直连多跳下的简化实现)。
func (d *datapath) forwardTransit(sessionID uint32, seq uint32, srcID, dstID [crypto.NodeIDSize]byte, ciphertext []byte) error {
	s, ok := d.sessions.Get(sessionID)
	if !ok {
		return fmt.Errorf("node: transit: no session %d", sessionID)
	}

	aad := buildAAD(srcID, dstID, sessionID)
	plaintext, err := s.Decrypt(uint64(seq), ciphertext, aad)
	if err != nil {
		return fmt.Errorf("node: transit: decrypt: %w", err)
	}

	rt := d.routeTable.Load()
	if rt == nil {
		return fmt.Errorf("node: transit: route table not ready")
	}
	entry, ok := rt.Lookup(dstID)
	if !ok {
		return fmt.Errorf("node: transit: no route to %s", crypto.NodeIDString(dstID))
	}
	if entry.NextHop == d.localID {
		// 不应发生:目的不是本节点却把自己作为下一跳。
		return fmt.Errorf("node: transit: next hop is self")
	}

	return d.sendViaHop(entry.NextHop, srcID, dstID, plaintext)
}

// sendViaHop 用 nextHop 的会话加密并发送一个端到端包。
//
// pktSrc/pkgDst 是端到端的源/目的(不随逐跳改变),用于构造 AAD 与
// ZCPacket 头,使最终接收方能校验来源与目的。
func (d *datapath) sendViaHop(nextHop, pktSrc, pktDst [crypto.NodeIDSize]byte, plaintext []byte) error {
	s, ok := d.sessions.GetByPeer(nextHop)
	if !ok || s.Closed() {
		return fmt.Errorf("node: send via hop: no session for %s", crypto.NodeIDString(nextHop))
	}
	addr, ok := d.peerAddr(nextHop)
	if !ok {
		return fmt.Errorf("node: send via hop: no address for %s", crypto.NodeIDString(nextHop))
	}

	aad := buildAAD(pktSrc, pktDst, s.ID)
	seq, ciphertext, err := s.SealNext(plaintext, aad)
	if err != nil {
		return fmt.Errorf("node: send via hop: seal: %w", err)
	}

	pkt := packet.New(ciphertext)
	pkt.SetType(packet.TypeData)
	pkt.SetFlags(packet.FlagEncrypted)
	pkt.SetSessionID(s.ID)
	pkt.SetSequence(uint32(seq))
	if err := pkt.SetSrcNodeID(pktSrc[:]); err != nil {
		return err
	}
	if err := pkt.SetDstNodeID(pktDst[:]); err != nil {
		return err
	}
	return d.sendRaw(pkt, addr)
}

// probeSender 返回一个把 Prober 的发送动作接到数据面控制包路径的适配器。
func (d *datapath) probeSender() route.ProbeSender {
	return &probeSender{dp: d}
}

// probeSender 适配 route.ProbeSender 到 datapath.sendControl。
type probeSender struct{ dp *datapath }

// SendProbe 发送一个探测包。
func (s *probeSender) SendProbe(peerID [crypto.NodeIDSize]byte, payload []byte) error {
	return s.dp.sendControl(peerID, packet.TypeProbe, payload)
}

// SendProbeAck 发送一个探测确认包。
func (s *probeSender) SendProbeAck(peerID [crypto.NodeIDSize]byte, payload []byte) error {
	return s.dp.sendControl(peerID, packet.TypeProbeAck, payload)
}

// ---------------------------------------------------------------------------
// 探测包处理
// ---------------------------------------------------------------------------

// handleProbe 处理收到的探测包:回送确认。
func (d *datapath) handleProbe(from net.Addr, pkt *packet.ZCPacket) {
	if d.prober == nil {
		return
	}
	srcID := toNodeID(pkt.SrcNodeID())
	// 探测载荷经会话加密,此处需要先解密。
	plaintext, sid, err := d.decryptControl(pkt)
	if err != nil {
		d.log.Debug("probe decrypt failed", zap.Error(err))
		return
	}
	_ = sid
	if err := d.prober.HandleProbe(srcID, plaintext, time.Now()); err != nil {
		d.log.Debug("handle probe", zap.String("from", from.String()), zap.Error(err))
	}
}

// handleProbeAck 处理收到的探测确认:更新链路质量。
func (d *datapath) handleProbeAck(from net.Addr, pkt *packet.ZCPacket) {
	if d.prober == nil {
		return
	}
	srcID := toNodeID(pkt.SrcNodeID())
	plaintext, _, err := d.decryptControl(pkt)
	if err != nil {
		d.log.Debug("probe ack decrypt failed", zap.Error(err))
		return
	}
	if err := d.prober.HandleProbeAck(srcID, plaintext, time.Now()); err != nil {
		d.log.Debug("handle probe ack", zap.String("from", from.String()), zap.Error(err))
	}
}

// decryptControl 解密一个直接邻居发来的控制包。
func (d *datapath) decryptControl(pkt *packet.ZCPacket) ([]byte, uint32, error) {
	sid := pkt.SessionID()
	s, ok := d.sessions.Get(sid)
	if !ok {
		return nil, 0, fmt.Errorf("no session %d", sid)
	}
	srcID := toNodeID(pkt.SrcNodeID())
	dstID := toNodeID(pkt.DstNodeID())
	aad := buildAAD(srcID, dstID, sid)
	plaintext, err := s.Decrypt(uint64(pkt.Sequence()), pkt.Payload(), aad)
	if err != nil {
		return nil, 0, err
	}
	return plaintext, sid, nil
}

// ---------------------------------------------------------------------------
// 定时任务
// ---------------------------------------------------------------------------

// runLSAFlooder 周期性泛洪本节点 LSA,直到 ctx 结束。
func (d *datapath) runLSAFlooder(ctx context.Context) {
	interval := d.cfg.lsaFloodEvery
	if interval <= 0 {
		interval = route.ProbeInterval * 30
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 启动时立即发布一次。
	d.broadcastSelfLSA()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.broadcastSelfLSA()
		}
	}
}

// runLSAAger 周期性老化拓扑库,直到 ctx 结束。
//
// 老化步长取 LSA 最大存活时间的 1/6(至少 100ms,至多 30s):这样一条
// 不再刷新的 LSA 会在约 maxAge 时间内被清除,而步长足够细,收敛延迟
// 控制在 maxAge/6 以内。
func (d *datapath) runLSAAger(ctx context.Context) {
	step := ageStepFor(d.cfg.lsaMaxAge)
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.stopCh:
			return
		case <-ticker.C:
			secs := uint16(step / time.Second)
			if secs == 0 {
				// 亚秒级步长(测试):按毫秒累积,避免 uint16 秒粒度丢失。
				secs = 1
			}
			if removed := d.topology.Age(secs); removed > 0 {
				d.recomputeRoutes()
			}
		}
	}
}

// ageStepFor 依据 LSA 最大存活时间推导老化步长。
func ageStepFor(maxAge time.Duration) time.Duration {
	if maxAge <= 0 {
		maxAge = 300 * time.Second
	}
	step := maxAge / 6
	if step < 100*time.Millisecond {
		step = 100 * time.Millisecond
	}
	if step > 30*time.Second {
		step = 30 * time.Second
	}
	return step
}
