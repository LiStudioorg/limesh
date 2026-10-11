// Package api 实现 Limesh 的 Admin API:一个基于 HTTP 的只读(加少量写操作)
// 管理接口,供 CLI、脚本与未来的 Web 面板消费。
//
// 传输方式:
//   - Unix Socket(默认,Linux/macOS):权限 0600,天然仅本机可达,无需 token;
//   - TCP(Windows 默认,或显式配置):强制 Bearer token 认证。
//
// 本包只做 HTTP 编解码与鉴权,所有数据来自注入的 NodeInfo 只读视图,
// 因此不依赖 core/node,便于测试与解耦。
package api

import (
	"time"

	"github.com/LiStudioorg/limesh/core/config"
)

// NodeInfo 是节点暴露给 API 的只读视图。
//
// 实现方(core/node.Node)负责把内部状态转换为稳定的快照类型,使 API 的
// 响应格式与内部实现解耦。
type NodeInfo interface {
	// Status 返回节点整体状态快照。
	Status() StatusSnapshot
	// PeerList 返回全部对端快照。
	PeerList() []PeerSnapshot
	// RouteList 返回转发表快照。
	RouteList() []RouteSnapshot
	// TopologySnapshot 返回全网拓扑快照。
	TopologySnapshot() TopologySnapshot
	// Config 返回当前生效的配置快照。
	Config() *config.Config
}

// StatusSnapshot 是节点状态的只读快照。
type StatusSnapshot struct {
	// NodeID 是本节点身份(十六进制)。
	NodeID string `json:"node_id"`
	// Hostname 是本节点主机名。
	Hostname string `json:"hostname"`
	// VirtualIP 是本节点虚拟 IP。
	VirtualIP string `json:"virtual_ip"`
	// Uptime 是进程已运行时长(秒)。
	UptimeSeconds int64 `json:"uptime_seconds"`
	// PeerCount 是对端总数。
	PeerCount int `json:"peer_count"`
	// DirectPeers 是处于直连状态的对端数。
	DirectPeers int `json:"direct_peers"`
	// RelayedPeers 是处于中继状态的对端数。
	RelayedPeers int `json:"relayed_peers"`
	// RouteCount 是路由表条目数。
	RouteCount int `json:"route_count"`
	// Version 是二进制版本。
	Version string `json:"version"`
	// IngressMode 是实际生效的接入模式(tun/no-tun/空)。
	IngressMode string `json:"ingress_mode"`
	// StartedAt 是节点启动时间。
	StartedAt time.Time `json:"started_at"`
}

// PeerSnapshot 是单个对端的只读快照。
type PeerSnapshot struct {
	// NodeID 是对端身份(十六进制)。
	NodeID string `json:"node_id"`
	// VirtualIP 是对端虚拟 IP。
	VirtualIP string `json:"virtual_ip"`
	// State 是 Peer 状态机状态名。
	State string `json:"state"`
	// RTTMillis 是平滑 RTT(毫秒);无样本时为 0。
	RTTMillis float64 `json:"rtt_ms"`
	// LossRate 是丢包率 [0,1]。
	LossRate float64 `json:"loss_rate"`
	// LastSeen 是最近一次收到对端包的时间。
	LastSeen time.Time `json:"last_seen"`
	// Addr 是对端地址(host:port)。
	Addr string `json:"addr"`
	// SessionID 是活跃会话 ID;无会话时为 0。
	SessionID uint32 `json:"session_id"`
}

// RouteSnapshot 是单条路由的只读快照。
type RouteSnapshot struct {
	// Dst 是目的节点身份(十六进制)。
	Dst string `json:"dst"`
	// DstVirtualIP 是目的虚拟 IP。
	DstVirtualIP string `json:"dst_virtual_ip"`
	// NextHop 是下一跳节点身份(十六进制)。
	NextHop string `json:"next_hop"`
	// Cost 是路径累计开销。
	Cost uint32 `json:"cost"`
	// Hops 是跳数。
	Hops uint8 `json:"hops"`
}

// TopologySnapshot 是全网拓扑的只读快照。
type TopologySnapshot struct {
	// Nodes 是全部已知节点。
	Nodes []NodeSnapshot `json:"nodes"`
	// Edges 是全部已知链路(有向)。
	Edges []EdgeSnapshot `json:"edges"`
}

// NodeSnapshot 是拓扑中的一个节点。
type NodeSnapshot struct {
	// NodeID 是节点身份(十六进制)。
	NodeID string `json:"node_id"`
	// VirtualIP 是虚拟 IP。
	VirtualIP string `json:"virtual_ip"`
	// IsSelf 标记是否为当前节点。
	IsSelf bool `json:"is_self"`
}

// EdgeSnapshot 是拓扑中的一条有向链路。
type EdgeSnapshot struct {
	// From 是链路起点节点身份(十六进制)。
	From string `json:"from"`
	// To 是链路终点节点身份(十六进制)。
	To string `json:"to"`
	// Cost 是链路开销。
	Cost uint32 `json:"cost"`
}
