package node

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/nat"
)

// natDetectInterval 是 NAT 探测/候选刷新的周期。
//
// 与配置的 detect_cache_ttl 对齐:每过一个缓存周期重新探测一次,保证映射
// 变化(NAT 重绑定)能被及时感知。
const natDetectInterval = 5 * time.Minute

// setupNAT 按配置构造 NAT 探测器与候选收集器。
//
// 未配置 STUN 服务器或少于 2 个时,探测器为空(只收集 host 候选)。
func (n *Node) setupNAT(opts Options) error {
	rc := n.cfg.NAT

	detector := opts.NATDetector
	if detector == nil && len(rc.STUNServers) >= 2 {
		d, err := nat.NewDetector(rc.STUNServers)
		if err != nil {
			return fmt.Errorf("new detector: %w", err)
		}
		d.SetTimeout(rc.STUNTimeout)
		d.SetCacheTTL(rc.DetectCacheTTL)
		detector = d
	}
	n.natDetector = detector

	collector, err := nat.NewCollector(nat.CollectorOptions{
		Config:   n.cfg,
		Detector: detector,
		Logger:   n.log,
	})
	if err != nil {
		return fmt.Errorf("new collector: %w", err)
	}
	n.natCollector = collector
	return nil
}

// runNATLoop 周期执行 NAT 探测与候选收集,直到 ctx 结束。
func (n *Node) runNATLoop(ctx context.Context) {
	if n.natCollector == nil {
		return
	}

	// 启动时立即执行一次。
	n.refreshNAT(ctx)

	ticker := time.NewTicker(natDetectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.refreshNAT(ctx)
		}
	}
}

// refreshNAT 执行一次 NAT 探测并刷新候选地址。
//
// 失败不视为致命:记录日志并保留上一次结果(或 unknown)。
func (n *Node) refreshNAT(ctx context.Context) {
	// 候选收集内部会调用探测(若已配置 detector)。
	cands, err := n.natCollector.Collect(ctx)
	if err != nil {
		n.log.Debug("candidate collection failed", zap.Error(err))
		return
	}

	var (
		ntype  = nat.NATUnknown
		mapped *nat.MappedAddr
	)
	if n.natDetector != nil {
		res, derr := n.natDetector.Detect(ctx)
		if derr != nil {
			n.log.Debug("nat detection failed", zap.Error(derr))
		}
		if res != nil {
			ntype = res.Type
			mapped = res.Mapped
		}
	}

	n.natMu.Lock()
	n.natType = ntype
	n.natMapped = mapped
	n.natDetected = time.Now()
	n.natCands = cands
	n.natMu.Unlock()

	n.log.Info("nat refreshed",
		zap.String("type", string(ntype)),
		zap.String("mapped", mappedString(mapped)),
		zap.Int("candidates", len(cands)),
	)
}

// mappedString 安全地把映射地址转为字符串。
func mappedString(m *nat.MappedAddr) string {
	if m == nil {
		return ""
	}
	return m.String()
}

// NATType 返回最近一次探测出的 NAT 类型;未探测时返回 unknown。
func (n *Node) NATType() nat.NATType {
	n.natMu.RLock()
	defer n.natMu.RUnlock()
	if n.natType == "" {
		return nat.NATUnknown
	}
	return n.natType
}

// MappedAddr 返回最近一次探测出的公网映射地址;无结果时返回 nil。
func (n *Node) MappedAddr() *nat.MappedAddr {
	n.natMu.RLock()
	defer n.natMu.RUnlock()
	return n.natMapped
}

// CandidateList 返回最近一次收集到的候选地址副本(内部类型)。
//
// 面向 API/CLI 的快照见 Candidates();此处返回 core/nat 的原始类型,
// 供需要构造打洞请求的内部逻辑使用(Phase 2b)。
func (n *Node) CandidateList() []nat.Candidate {
	n.natMu.RLock()
	defer n.natMu.RUnlock()
	return append([]nat.Candidate(nil), n.natCands...)
}

// NATDetectedAt 返回最近一次 NAT 探测的时间;未探测时为零值。
func (n *Node) NATDetectedAt() time.Time {
	n.natMu.RLock()
	defer n.natMu.RUnlock()
	return n.natDetected
}

// ProbeNAT 同步执行一次 NAT 探测并刷新候选。
//
// 供 Admin API 的 `POST /api/v1/nat/probe`(即 CLI `mesh nat probe`)与测试
// 使用;节点运行时通常由 runNATLoop 周期调用。
//
// 返回错误表示未启用 NAT 或重探未能获得任何可用结果;调用方随后可通过
// NATType()/MappedAddr()/Candidates() 读取刷新后的快照。
func (n *Node) ProbeNAT(ctx context.Context) error {
	if n.natCollector == nil {
		return fmt.Errorf("node: nat not enabled")
	}
	n.refreshNAT(ctx)
	return nil
}
