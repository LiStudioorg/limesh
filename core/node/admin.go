package node

import (
	"time"

	"github.com/LiStudioorg/limesh/core/api"
	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/peer"
)

// 本文件实现 api.NodeInfo:把节点内部状态转换为 API 的稳定快照。
//
// 转换集中在此处的好处:HTTP 层与内部实现的耦合被限制在一个文件内,
// 内部结构演进时只需调整这里,API 契约保持稳定。

// Status 实现 api.NodeInfo。
func (n *Node) Status() api.StatusSnapshot {
	snap := api.StatusSnapshot{
		Hostname:      n.Hostname,
		UptimeSeconds: int64(time.Since(n.startedAt).Seconds()),
		Version:       n.version,
		StartedAt:     n.startedAt,
	}

	if n.dp == nil {
		return snap
	}

	id := n.dp.LocalNodeID()
	snap.NodeID = crypto.NodeIDString(id)
	snap.VirtualIP = crypto.VirtualIPString(id, true)

	peers := n.dp.Peers().All()
	snap.PeerCount = len(peers)
	for _, p := range peers {
		switch p.State() {
		case peer.StateDirect:
			snap.DirectPeers++
		case peer.StateRelayed:
			snap.RelayedPeers++
		}
	}
	if n.dp.RouteTable() != nil {
		snap.RouteCount = n.dp.RouteTable().Len()
	}
	if n.ingress != nil {
		snap.IngressMode = string(n.ingress.Mode())
	}
	return snap
}

// PeerList 实现 api.NodeInfo。
func (n *Node) PeerList() []api.PeerSnapshot {
	if n.dp == nil {
		return []api.PeerSnapshot{}
	}

	peers := n.dp.Peers().All()
	out := make([]api.PeerSnapshot, 0, len(peers))
	for _, p := range peers {
		s := api.PeerSnapshot{
			NodeID:    p.NodeIDHex(),
			VirtualIP: p.VirtualIP(),
			State:     p.State().String(),
			Addr:      p.Addr(),
			LastSeen:  p.LastSeen(),
		}
		if rtt, ok := p.RTT(); ok {
			s.RTTMillis = float64(rtt.Microseconds()) / 1000.0
		}
		if n.dp.prober != nil {
			st := n.dp.prober.Stats(p.NodeID)
			s.LossRate = st.LossRate
		}
		if sess := p.Session(); sess != nil {
			s.SessionID = sess.ID
		}
		out = append(out, s)
	}
	return out
}

// RouteList 实现 api.NodeInfo。
func (n *Node) RouteList() []api.RouteSnapshot {
	if n.dp == nil || n.dp.RouteTable() == nil {
		return []api.RouteSnapshot{}
	}

	entries := n.dp.RouteTable().Dump()
	out := make([]api.RouteSnapshot, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.RouteSnapshot{
			Dst:          crypto.NodeIDString(e.Dst),
			DstVirtualIP: crypto.VirtualIPString(e.Dst, true),
			NextHop:      crypto.NodeIDString(e.NextHop),
			Cost:         e.Cost,
			Hops:         e.Hops,
		})
	}
	return out
}

// TopologySnapshot 实现 api.NodeInfo。
func (n *Node) TopologySnapshot() api.TopologySnapshot {
	snap := api.TopologySnapshot{
		Nodes: []api.NodeSnapshot{},
		Edges: []api.EdgeSnapshot{},
	}
	if n.dp == nil || n.dp.Topology() == nil {
		return snap
	}

	self := n.dp.LocalNodeID()
	for _, lsa := range n.dp.Topology().Snapshot() {
		snap.Nodes = append(snap.Nodes, api.NodeSnapshot{
			NodeID:    crypto.NodeIDString(lsa.Origin),
			VirtualIP: crypto.VirtualIPString(lsa.Origin, true),
			IsSelf:    lsa.Origin == self,
		})
		for _, nb := range lsa.Neighbors {
			snap.Edges = append(snap.Edges, api.EdgeSnapshot{
				From: crypto.NodeIDString(lsa.Origin),
				To:   crypto.NodeIDString(nb.NodeID),
				Cost: nb.Cost,
			})
		}
	}
	return snap
}

// NAT 实现 api.NodeInfo。
func (n *Node) NAT() api.NATSnapshot {
	snap := api.NATSnapshot{
		Type: string(n.NATType()),
	}
	if m := n.MappedAddr(); m != nil {
		snap.Mapped = m.String()
	}
	snap.DetectedAt = n.NATDetectedAt()
	snap.CandidateCount = len(n.Candidates())
	return snap
}

// Candidates 实现 api.NodeInfo。
func (n *Node) Candidates() []api.CandidateSnapshot {
	cands := n.CandidateList()
	out := make([]api.CandidateSnapshot, 0, len(cands))
	for _, c := range cands {
		out = append(out, api.CandidateSnapshot{
			Type:     string(c.Type),
			Addr:     c.Addr.String(),
			Priority: c.Priority,
			Source:   c.Source,
		})
	}
	return out
}

// 确保 Node 满足 api.NodeInfo(编译期断言)。
var _ api.NodeInfo = (*Node)(nil)
