// Package node 实现 Limesh 节点的最小装配。
//
// 本包刻意保持零 CLI 依赖:不 import cobra / pflag,不读取 os.Args。
// 所有配置经由结构体注入,使同一套逻辑可用于 CLI、移动端桥接与嵌入式场景
// (见 AGENTS.md 3.2 节)。
package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/api"
	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/ingress"
	"github.com/LiStudioorg/limesh/core/nat"
	"github.com/LiStudioorg/limesh/core/packet"
	"github.com/LiStudioorg/limesh/core/peer"
	"github.com/LiStudioorg/limesh/core/route"
	"github.com/LiStudioorg/limesh/core/session"
	"github.com/LiStudioorg/limesh/core/transport"
)

// packetFromPlaintext 把一段 IP 明文包装成 Ingress 可消费的 ZCPacket。
//
// Deliver 约定其入参的 Payload() 是 IP 包,故此处的 ZCPacket 仅作为载体;
// 它不携带真实会话头(接入层不感知会话)。packet.New 会拷贝载荷,因此
// 调用方传入的切片在返回后即可释放。
func packetFromPlaintext(plaintext []byte) *packet.ZCPacket {
	pkt := packet.New(plaintext)
	pkt.SetType(packet.TypeData)
	return pkt
}

// 节点错误。
var (
	// ErrAlreadyRunning 表示 Run 被重复调用。
	ErrAlreadyRunning = errors.New("node: already running")
	// ErrNoListener 表示没有任何监听器成功启动。
	ErrNoListener = errors.New("node: no listener started")
	// ErrNoIngress 表示节点未装配接入层,无法转发 IP 包。
	ErrNoIngress = errors.New("node: no ingress configured")
)

// Node 是一个最小可用的 Limesh 节点。
//
// Phase 0 的职责边界:持有配置与传输实例,按配置启动监听,并阻塞运行至
// 上下文取消。它不做握手、不做加密、不做路由——这些属于后续阶段。
type Node struct {
	cfg *config.Config
	log *zap.Logger

	transport transport.Transport
	listeners []transport.Listener

	mu      sync.Mutex
	running bool
	closed  bool

	// Hostname 是本节点的有效主机名(配置为空时由调用方提供的回退值)。
	Hostname string

	// dp 是加密数据面。未配置长期密钥时为 nil,此时节点退化为 Phase 0 的
	// 裸传输行为(仅监听,不握手)。
	dp *datapath
	// keyPair 是本节点长期身份密钥。
	keyPair *crypto.KeyPair
	// peerSpecs 是待连接的已知对端(地址 + 公钥)。
	peerSpecs []PeerSpec
	// ingress 是接入层(TUN)。为空时节点只做加密数据面,不接入 IP 包。
	ingress ingress.Ingress
	// startedAt 是节点启动时间,用于 uptime。
	startedAt time.Time
	// adminSrv 是 Admin API 服务端;未启用时为 nil。
	adminSrv *api.Server
	// skipAdmin 为真时跳过 API 启动(测试用途)。
	skipAdmin bool
	// version 是二进制版本(注入给 Admin API)。
	version string
	// reload 触发配置热重载(注入给 Admin API);可为 nil。
	reload func() error

	// ---- NAT 穿透(Phase 2a) ----

	// natDetector 探测 NAT 类型;为 nil 表示未启用。
	natDetector *nat.Detector
	// natCollector 收集候选地址;为 nil 表示未启用。
	natCollector *nat.Collector

	// natMu 保护下面的 NAT 观测结果。
	natMu       sync.RWMutex
	natType     nat.NATType
	natMapped   *nat.MappedAddr
	natDetected time.Time
	natCands    []nat.Candidate
}

// Options 是构造 Node 的参数。
type Options struct {
	// Config 是合并后的配置快照,必填。
	Config *config.Config
	// Logger 是日志实例;为空时使用 zap.NewNop()。
	Logger *zap.Logger
	// Hostname 是配置未指定主机名时的回退值。
	// core 包不读取系统 hostname,由 CLI 层传入以保持环境无关。
	Hostname string
	// Transport 允许注入自定义传输实现(测试用途)。
	// 为空时按配置中的监听协议自动构造。
	Transport transport.Transport
	// KeyPair 是本节点长期身份密钥。为空时退化为无加密数据面的裸节点。
	KeyPair *crypto.KeyPair
	// Peers 是已知对端列表(地址 + 公钥)。用于在 Run 时主动发起握手。
	Peers []PeerSpec
	// OnPlaintext 是数据包解密成功后的回调。为空时明文被丢弃。
	//
	// 当装配了 Ingress 时,Ingress 优先接管明文投递,本回调不再被调用
	// (避免同一明文被两个消费者重复处理)。
	//
	// 回调在数据面读循环内同步调用,实现必须尽快返回。
	OnPlaintext func(srcID [crypto.NodeIDSize]byte, plaintext []byte)
	// Ingress 允许注入接入层实现(测试用途)。
	// 为空且配置了密钥时,按配置自动构造接入层。
	Ingress ingress.Ingress
	// NoIngress 为真时跳过接入层构造,仅运行加密数据面(Phase 1a 行为)。
	// 用于只需要握手/加密、不需要 IP 转发的场景(如现有 E2E 测试)。
	NoIngress bool
	// NoAdmin 为真时即使配置启用也跳过 Admin API(测试用途)。
	NoAdmin bool
	// NoNAT 为真时跳过 NAT 探测与候选收集(测试用途)。
	NoNAT bool
	// NATDetector 允许注入 NAT 探测器(测试用途);为空时按配置构造。
	NATDetector *nat.Detector
	// Version 是二进制版本,用于 Admin API 的 /version 端点。
	Version string
	// Reload 触发配置热重载;为空时 Admin API 的 reload 端点返回 501。
	Reload func() error
}

// New 按配置装配一个节点。
//
// 装配阶段只做校验与构造,不产生网络副作用;监听在 Run 中建立。
func New(opts Options) (*Node, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("node: new: config must not be nil")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("node: new: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}

	node := &Node{
		cfg:       opts.Config,
		log:       log,
		Hostname:  opts.Config.HostnameOr(opts.Hostname),
		keyPair:   opts.KeyPair,
		peerSpecs: append([]PeerSpec(nil), opts.Peers...),
		startedAt: time.Now(),
		skipAdmin: opts.NoAdmin,
		version:   opts.Version,
		reload:    opts.Reload,
	}

	// 接入层:优先使用注入的实现;否则按配置构造(可用 NoIngress 跳过)。
	if opts.Ingress != nil {
		node.ingress = opts.Ingress
	} else if opts.KeyPair != nil && !opts.NoIngress {
		ing, err := ingress.New(ingress.Options{
			Config: opts.Config,
			Logger: log,
		})
		if err != nil {
			return nil, fmt.Errorf("node: new: ingress: %w", err)
		}
		node.ingress = ing
	}

	if opts.KeyPair != nil {
		rc := opts.Config.Routing
		cv := &configView{
			networkName:   opts.Config.Network.Name,
			prologue:      []byte(opts.Config.Network.Name),
			e2ee:          opts.Config.Security.E2EE,
			strategy:      rc.Strategy,
			ewmaAlpha:     rc.EWMAAlpha,
			lsaFloodEvery: rc.LSAFloodInterval,
			lsaMaxAge:     rc.LSAMaxAge,
			probeInterval: rc.ProbeInterval,
		}
		node.dp = newDatapath(opts.KeyPair, cv, log)

		// 链路质量探测:发送器复用数据面的控制包发送路径。
		prober, err := route.NewProber(route.ProberOptions{
			Sender:    node.dp.probeSender(),
			Interval:  rc.ProbeInterval,
			EWMAAlpha: rc.EWMAAlpha,
		})
		if err != nil {
			return nil, fmt.Errorf("node: new: prober: %w", err)
		}
		node.dp.prober = prober

		// NAT 穿透(Phase 2a):探测 NAT 类型并收集候选地址。未配置 STUN 或
		// 显式禁用时不启用,节点仍可正常组网(只是缺少 srflx 候选与 NAT 类型)。
		if !opts.NoNAT {
			if err := node.setupNAT(opts); err != nil {
				return nil, fmt.Errorf("node: new: nat: %w", err)
			}
		}

		// 握手时携带本端候选地址。
		node.dp.candidateProvider = node.CandidateList

		// 明文投递:装配了 Ingress 时交给 Ingress 写回 TUN;否则用注入的回调。
		switch {
		case node.ingress != nil:
			ing := node.ingress
			node.dp.setPlaintextHandler(func(_ [crypto.NodeIDSize]byte, plaintext []byte) {
				ing.Deliver(packetFromPlaintext(plaintext))
			})
		case opts.OnPlaintext != nil:
			node.dp.setPlaintextHandler(opts.OnPlaintext)
		}
	}

	tr := opts.Transport
	if tr == nil {
		scheme := transport.TransportType(opts.Config.Listen[0].Scheme())
		t, err := transport.New(scheme, transport.Options{})
		if err != nil {
			return nil, fmt.Errorf("node: new: %w", err)
		}
		tr = t
	}
	node.transport = tr

	return node, nil
}

// Config 返回节点持有的配置快照。
func (n *Node) Config() *config.Config { return n.cfg }

// Transport 返回节点使用的传输实例。
func (n *Node) Transport() transport.Transport { return n.transport }

// Ingress 返回节点装配的接入层;未装配时返回 nil。
func (n *Node) Ingress() ingress.Ingress { return n.ingress }

// Listeners 返回已启动的监听器。Run 之前为空。
func (n *Node) Listeners() []transport.Listener {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]transport.Listener(nil), n.listeners...)
}

// Addrs 返回所有监听器的实际地址。
//
// 配置里写端口 0 时实际端口由内核分配,该方法是获取真实端口的唯一途径。
func (n *Node) Addrs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	out := make([]string, 0, len(n.listeners))
	for _, l := range n.listeners {
		out = append(out, l.Addr().String())
	}
	return out
}

// Run 启动监听并阻塞运行,直到 ctx 被取消或 Close 被调用。
//
// 返回 nil 表示正常退出;任何监听器失败都会关闭已启动的监听器并返回错误,
// 不会留下半启动状态。
func (n *Node) Run(ctx context.Context) error {
	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		return ErrAlreadyRunning
	}
	if n.closed {
		n.mu.Unlock()
		return fmt.Errorf("node: run: %w", transport.ErrClosed)
	}
	n.running = true
	n.mu.Unlock()

	n.log.Info("mesh node starting",
		zap.String("name", n.cfg.Network.Name),
		zap.String("hostname", n.Hostname),
	)

	// 启动全部监听器。
	started, err := n.startListeners()
	if err != nil {
		n.closeListeners(started)
		n.mu.Lock()
		n.running = false
		n.mu.Unlock()
		return err
	}

	n.mu.Lock()
	n.listeners = started
	n.mu.Unlock()

	for _, l := range started {
		n.log.Info("listening",
			zap.String("transport", string(n.transport.Type())),
			zap.String("addr", l.Addr().String()),
		)
	}

	// 若已配置长期密钥,则启动加密数据面。
	dpDone := n.startDatapath(ctx, started)

	// 接入层 → 数据面的转发循环:把 TUN 上读到的 IP 包加密后发给对端。
	fwdDone := n.startIngressForwarding(ctx)

	// 启动 Admin API(若配置启用且未被测试禁用)。
	adminDone := n.startAdminAPI(ctx)

	// 启动 NAT 探测与候选收集循环(若已装配)。
	natDone := n.startNATLoop(ctx)

	ingressMode := ""
	if n.ingress != nil {
		ingressMode = string(n.ingress.Mode())
	}

	n.log.Info("mesh node running",
		zap.String("name", n.cfg.Network.Name),
		zap.String("hostname", n.Hostname),
		zap.Int("listeners", len(started)),
		zap.Strings("peers", n.cfg.Peers),
		zap.Bool("encrypted", n.dp != nil),
		zap.String("ingress", ingressMode),
	)

	// 阻塞至上下文取消。
	<-ctx.Done()

	n.mu.Lock()
	n.running = false
	n.mu.Unlock()

	n.log.Info("mesh node stopping",
		zap.String("name", n.cfg.Network.Name),
		zap.String("reason", context.Cause(ctx).Error()),
	)

	// 关闭顺序:Admin API → NAT → 转发循环 → 数据面 → 监听器。
	if n.adminSrv != nil {
		_ = n.adminSrv.Close()
	}
	if adminDone != nil {
		<-adminDone
	}
	if natDone != nil {
		<-natDone
	}
	if fwdDone != nil {
		<-fwdDone
	}
	if dpDone != nil {
		<-dpDone
	}

	if err := n.Close(); err != nil {
		return fmt.Errorf("node: run: shutdown: %w", err)
	}
	return nil
}

// startAdminAPI 按配置启动 Admin API 服务端,返回其结束信号。
//
// 未启用、被测试禁用或构造失败时返回 nil;构造失败不阻断节点启动
// (管理接口不可用不应导致数据面不可用),但会记录警告。
func (n *Node) startAdminAPI(ctx context.Context) <-chan struct{} {
	if !n.cfg.Admin.Enabled || n.skipAdmin {
		return nil
	}

	srv, err := api.NewServer(api.ServerOptions{
		Config:  n.cfg,
		Node:    n,
		Logger:  n.log,
		Version: n.version,
		Reload:  n.reload,
	})
	if err != nil {
		n.log.Warn("admin api disabled: construct failed", zap.Error(err))
		return nil
	}
	if err := srv.Listen(); err != nil {
		n.log.Warn("admin api disabled: listen failed", zap.Error(err))
		return nil
	}
	n.adminSrv = srv

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			n.log.Warn("admin api serve error", zap.Error(err))
		}
	}()
	return done
}

// startNATLoop 启动 NAT 探测循环,返回其结束信号;未装配时返回 nil。
func (n *Node) startNATLoop(ctx context.Context) <-chan struct{} {
	if n.natCollector == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.runNATLoop(ctx)
	}()
	return done
}

// AdminAddr 返回 Admin API 的实际监听地址;未启用时为空。
func (n *Node) AdminAddr() string {
	if n.adminSrv == nil {
		return ""
	}
	return n.adminSrv.Addr()
}

// startIngressForwarding 启动"TUN → 数据面"的转发循环。
//
// 返回循环结束信号;未装配接入层或数据面时返回 nil。
//
// Phase 1b 采用最简路由:出站包一律发给唯一的活跃对端(DirectPeer)。
// 完整 OSPF 路由属 Phase 1c。
func (n *Node) startIngressForwarding(ctx context.Context) <-chan struct{} {
	if n.ingress == nil || n.dp == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.forwardIngressToDataPath(ctx)
	}()
	return done
}

// forwardIngressToDataPath 从接入层读取本机产生的 IP 包,加密后发给对端。
//
// 反方向(数据面 → 接入层)由数据面的明文回调驱动,见 New 中的装配。
func (n *Node) forwardIngressToDataPath(ctx context.Context) {
	out := n.ingress.Outbound()
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-out:
			if !ok {
				// 接入层已关闭(读循环退出)。
				return
			}
			n.forwardOne(pkt)
		}
	}
}

// forwardOne 处理一个来自 TUN 的 IP 包:解析目的节点 → 查路由 → 发送。
//
// Phase 1c 起不再硬编码单 peer:目的节点由 IP 头解析(反查虚拟 IP 映射),
// 下一跳由转发表决定,因此支持透明多跳(A → B → C)。
func (n *Node) forwardOne(pkt *packet.ZCPacket) {
	payload := pkt.Payload()
	if len(payload) == 0 {
		return
	}

	dst, err := n.dp.resolveDstNodeID(payload)
	if err != nil {
		n.log.Debug("ingress forward: cannot resolve destination",
			zap.Int("len", len(payload)), zap.Error(err))
		return
	}
	if dst == n.dp.localID {
		return
	}

	if err := n.dp.SendToNode(dst, payload); err != nil {
		n.log.Debug("ingress forward: send failed",
			zap.String("dst", crypto.NodeIDString(dst)), zap.Error(err))
	}
}

// startDatapath 把支持数据报的监听器接入数据面并启动它,返回其结束信号。
//
// 未配置密钥、或没有任何监听器支持数据报模式时返回 nil,节点保持 Phase 0
// 的裸传输行为。
func (n *Node) startDatapath(ctx context.Context, listeners []transport.Listener) <-chan struct{} {
	if n.dp == nil {
		return nil
	}

	datagrams := 0
	for _, l := range listeners {
		dg, ok := transport.AsDatagram(l)
		if !ok {
			continue
		}
		n.dp.addDatagram(dg)
		n.dp.registerDatagramAddr(l.Addr().String(), dg)
		datagrams++
	}
	if datagrams == 0 {
		n.log.Warn("no datagram-capable listener; encrypted data path disabled")
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.dp.run(ctx)
	}()

	// L5 路由后台任务:泛洪本节点 LSA、老化拓扑库、周期探测链路质量。
	go n.dp.runLSAFlooder(ctx)
	go n.dp.runLSAAger(ctx)
	if n.dp.prober != nil {
		go n.dp.prober.Run(ctx)
	}

	// 主动向已知对端发起握手。
	for _, spec := range n.peerSpecs {
		if err := n.dp.initiate(spec); err != nil {
			n.log.Warn("initiate handshake failed",
				zap.String("peer", spec.Addr), zap.Error(err))
		}
	}
	return done
}

// startListeners 按配置启动监听器,遇到错误时返回已成功启动的部分。
func (n *Node) startListeners() ([]transport.Listener, error) {
	started := make([]transport.Listener, 0, len(n.cfg.Listen))

	for i, lc := range n.cfg.Listen {
		scheme := transport.TransportType(lc.Scheme())

		// 传输实例按配置中的首个协议构造;多协议共存留待后续阶段。
		if scheme != n.transport.Type() {
			n.log.Warn("listen skipped: unsupported by active transport",
				zap.Int("index", i),
				zap.String("addr", lc.Addr),
				zap.String("scheme", string(scheme)),
				zap.String("active", string(n.transport.Type())),
			)
			continue
		}

		ln, err := n.transport.Listen(lc.HostPort())
		if err != nil {
			return started, fmt.Errorf("node: listen %q: %w", lc.Addr, err)
		}
		started = append(started, ln)
	}

	if len(started) == 0 {
		return nil, ErrNoListener
	}
	return started, nil
}

// closeListeners 关闭一组监听器,忽略单个关闭错误以保证全部被尝试。
func (n *Node) closeListeners(ls []transport.Listener) {
	for _, l := range ls {
		if err := l.Close(); err != nil {
			n.log.Warn("listener close failed", zap.Error(err))
		}
	}
}

// RuntimeStart 在后台启动节点并等待监听器就绪。
//
// 它是 Run 的非阻塞封装:Run 仍然阻塞直到 ctx 取消,这里是给需要"启动后
// 立即返回、自行控制生命周期"的调用方(典型是测试与嵌入式场景)使用的。
//
// 返回时监听器已启动;若启动失败,错误会原样返回,后台 goroutine 不会
// 遗留未处理的 panic。
func (n *Node) RuntimeStart(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	// 等待监听器出现;期间若 Run 提前返回错误,立即上报。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(n.Listeners()) > 0 {
			return nil
		}
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			return nil
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node: runtime start: listeners not ready")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// Sessions 返回会话池;未配置密钥时返回 nil。
func (n *Node) Sessions() *session.Manager {
	if n.dp == nil {
		return nil
	}
	return n.dp.Sessions()
}

// Peers 返回对端注册表;未配置密钥时返回 nil。
func (n *Node) Peers() *peer.Manager {
	if n.dp == nil {
		return nil
	}
	return n.dp.Peers()
}

// RouteTo 查询到目的 NodeID 的路由;未配置数据面时返回 false。
func (n *Node) RouteTo(dst [crypto.NodeIDSize]byte) (route.RouteEntry, bool) {
	if n.dp == nil {
		return route.RouteEntry{}, false
	}
	return n.dp.RouteTo(dst)
}

// RouteTable 返回当前转发表;未配置数据面时返回 nil。
func (n *Node) RouteTable() *route.RouteTable {
	if n.dp == nil {
		return nil
	}
	return n.dp.RouteTable()
}

// Topology 返回链路状态数据库;未配置数据面时返回 nil。
func (n *Node) Topology() *route.TopologyDB {
	if n.dp == nil {
		return nil
	}
	return n.dp.Topology()
}

// LocalNodeID 返回本节点身份;未配置密钥时返回零值。
func (n *Node) LocalNodeID() [crypto.NodeIDSize]byte {
	if n.dp == nil {
		return [crypto.NodeIDSize]byte{}
	}
	return n.dp.LocalNodeID()
}

// SendData 使用对端会话加密并发送一个数据包。
func (n *Node) SendData(peerID [crypto.NodeIDSize]byte, plaintext []byte) error {
	if n.dp == nil {
		return ErrNoKeyPair
	}
	return n.dp.SendData(peerID, plaintext)
}

// Close 关闭全部监听器。重复调用是安全的。
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	listeners := n.listeners
	n.listeners = nil
	dp := n.dp
	ing := n.ingress
	n.mu.Unlock()

	// 先关接入层:其读循环会结束并关闭 Outbound,使转发循环退出。
	if ing != nil {
		if err := ing.Close(); err != nil {
			n.log.Warn("ingress close failed", zap.Error(err))
		}
	}

	// 数据面可能在 Run 之外被直接关闭(例如构造后未 Run 即 Close),
	// 这里做幂等兜底。
	if dp != nil {
		dp.shutdown()
	}

	// 释放 NAT 探测器的 STUN socket。
	if n.natDetector != nil {
		_ = n.natDetector.Close()
	}

	n.closeListeners(listeners)
	return nil
}
