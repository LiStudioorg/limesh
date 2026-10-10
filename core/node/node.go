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

	"github.com/LiStudioorg/limesh/core/config"
	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/peer"
	"github.com/LiStudioorg/limesh/core/session"
	"github.com/LiStudioorg/limesh/core/transport"
)

// 节点错误。
var (
	// ErrAlreadyRunning 表示 Run 被重复调用。
	ErrAlreadyRunning = errors.New("node: already running")
	// ErrNoListener 表示没有任何监听器成功启动。
	ErrNoListener = errors.New("node: no listener started")
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
	// Phase 1a 用它把明文交付给测试或临时消费者;Ingress 接入后由 Ingress
	// 取代这一回调。回调在数据面读循环内同步调用,实现必须尽快返回。
	OnPlaintext func(srcID [crypto.NodeIDSize]byte, plaintext []byte)
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
	}

	if opts.KeyPair != nil {
		cv := &configView{
			networkName: opts.Config.Network.Name,
			prologue:    []byte(opts.Config.Network.Name),
			e2ee:        opts.Config.Security.E2EE,
		}
		node.dp = newDatapath(opts.KeyPair, cv, log)
		if opts.OnPlaintext != nil {
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

	n.log.Info("mesh node running",
		zap.String("name", n.cfg.Network.Name),
		zap.String("hostname", n.Hostname),
		zap.Int("listeners", len(started)),
		zap.Strings("peers", n.cfg.Peers),
		zap.Bool("encrypted", n.dp != nil),
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

	// 先停数据面再关监听器:数据面读循环依赖监听 socket 存活。
	if dpDone != nil {
		<-dpDone
	}

	if err := n.Close(); err != nil {
		return fmt.Errorf("node: run: shutdown: %w", err)
	}
	return nil
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
	n.mu.Unlock()

	// 数据面可能在 Run 之外被直接关闭(例如构造后未 Run 即 Close),
	// 这里做幂等兜底。
	if dp != nil {
		dp.shutdown()
	}

	n.closeListeners(listeners)
	return nil
}
