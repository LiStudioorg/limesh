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

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
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
		cfg:      opts.Config,
		log:      log,
		Hostname: opts.Config.HostnameOr(opts.Hostname),
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

	n.log.Info("mesh node running",
		zap.String("name", n.cfg.Network.Name),
		zap.String("hostname", n.Hostname),
		zap.Int("listeners", len(started)),
		zap.Strings("peers", n.cfg.Peers),
	)

	// 阻塞至上下文取消。Phase 0 尚无后台任务,因此只需等待信号。
	<-ctx.Done()

	n.mu.Lock()
	n.running = false
	n.mu.Unlock()

	n.log.Info("mesh node stopping",
		zap.String("name", n.cfg.Network.Name),
		zap.String("reason", context.Cause(ctx).Error()),
	)

	if err := n.Close(); err != nil {
		return fmt.Errorf("node: run: shutdown: %w", err)
	}
	return nil
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
	n.mu.Unlock()

	n.closeListeners(listeners)
	return nil
}
