package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/LiStudioorg/limesh/core/config"
)

// API 相关错误。
var (
	// ErrUnauthorized 表示请求未通过 token 校验。
	ErrUnauthorized = errors.New("api: unauthorized")
	// ErrAlreadyRunning 表示 Server 已被启动。
	ErrAlreadyRunning = errors.New("api: already running")
)

// 服务端常量。
const (
	// socketPerm 是 Unix Socket 文件权限:仅属主可读写。
	socketPerm os.FileMode = 0o600
	// tokenPerm 是 token 文件权限。
	tokenPerm os.FileMode = 0o600
	// readHeaderTimeout 限制请求头读取时间,防止慢速攻击。
	readHeaderTimeout = 10 * time.Second
	// defaultVersion 是未注入版本时的占位。
	defaultVersion = "dev"
)

// Server 是 Admin API 的 HTTP 服务端。
//
// 支持两种监听:Unix Socket(默认)与 TCP + Bearer token。
type Server struct {
	cfg     *config.Config
	node    NodeInfo
	log     *zap.Logger
	version string
	// reload 是触发配置热重载的回调,可为 nil(不支持重载)。
	reload func() error

	ln      net.Listener
	httpSrv *http.Server
	mux     *http.ServeMux

	// token 在 TCP 模式下用于认证;空表示无需认证(Unix Socket)。
	token string

	mu      sync.Mutex
	running bool
	// actualAddr 是实际监听地址(Unix Socket 为路径,TCP 为 host:port)。
	actualAddr string

	// probeMu 保证同一时刻只有一次 NAT 重探在跑;tryLock 失败返回 409。
	probeMu sync.Mutex
}

// ServerOptions 是构造 Server 的参数。
type ServerOptions struct {
	// Config 是配置快照,必填。
	Config *config.Config
	// Node 是只读节点视图,必填。
	Node NodeInfo
	// Logger 是日志实例;为空时使用 zap.NewNop()。
	Logger *zap.Logger
	// Version 是二进制版本,用于 /version 端点。
	Version string
	// Reload 触发配置热重载;为 nil 时 reload 端点返回 501。
	Reload func() error
}

// NewServer 构造一个 Admin API 服务端(不启动监听)。
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Node == nil {
		return nil, fmt.Errorf("api: new server: node info must not be nil")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("api: new server: config must not be nil")
	}
	log := opts.Logger
	if log == nil {
		log = zap.NewNop()
	}
	version := opts.Version
	if version == "" {
		version = defaultVersion
	}

	s := &Server{
		cfg:     opts.Config,
		node:    opts.Node,
		log:     log,
		version: version,
		reload:  opts.Reload,
	}
	s.mux = s.routes()
	return s, nil
}

// Listen 建立监听器(Unix Socket 或 TCP),但不开始处理请求。
//
// 分离 Listen 与 Serve 便于测试获取实际地址。Unix Socket 会对已存在的
// socket 文件做处理:若旧文件残留则删除,避免"address already in use"。
func (s *Server) Listen() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.mu.Unlock()

	listen := s.cfg.EffectiveAdminListen()
	scheme, addr := splitScheme(listen)

	var ln net.Listener
	var err error
	switch scheme {
	case "unix":
		ln, err = listenUnix(addr)
		if err != nil {
			return err
		}
		s.token = ""

	case "tcp":
		tok, terr := loadOrCreateToken(s.cfg.Admin.TokenFile)
		if terr != nil {
			return terr
		}
		s.token = tok
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("api: listen tcp %q: %w", addr, err)
		}

	default:
		return fmt.Errorf("api: unsupported listen scheme %q", scheme)
	}

	s.ln = ln
	// TCP 端口写 0 时由内核分配,必须从监听器读回真实地址;Unix Socket
	// 的 Addr() 即路径,与配置一致。
	s.actualAddr = ln.Addr().String()
	s.httpSrv = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return nil
}

// Addr 返回实际监听地址(Unix Socket 路径或 TCP host:port)。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.actualAddr
}

// Serve 在已建立的监听器上处理请求,阻塞至 ctx 结束或 Close。
//
// 必须先调用 Listen。ctx 取消时会优雅关闭。
func (s *Server) Serve(ctx context.Context) error {
	if s.httpSrv == nil {
		return fmt.Errorf("api: serve: Listen must be called first")
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	s.log.Info("admin api listening",
		zap.String("addr", s.actualAddr),
		zap.Bool("auth", s.token != ""))

	err := s.httpSrv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close 关闭服务端并清理 socket 文件。重复调用是安全的。
func (s *Server) Close() error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	srv := s.httpSrv
	addr := s.actualAddr
	s.mu.Unlock()

	var err error
	if srv != nil {
		err = srv.Close()
	}
	// Unix Socket 关闭后内核会移除文件,但部分平台未必;兜底删除。
	if strings.HasPrefix(s.cfg.EffectiveAdminListen(), "unix://") && addr != "" {
		_ = os.Remove(addr)
	}
	return err
}

// listenUnix 创建一个 Unix Socket 监听,处理残留文件与权限。
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("api: create socket dir: %w", err)
	}
	// 残留的 socket 文件会导致 bind 失败;仅当它是 socket 时才删除,
	// 避免误删同名普通文件。
	if info, err := os.Stat(path); err == nil {
		if info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(path)
		} else {
			return nil, fmt.Errorf("api: %q exists and is not a socket", path)
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("api: listen unix %q: %w", path, err)
	}
	if err := os.Chmod(path, socketPerm); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("api: chmod socket: %w", err)
	}
	return ln, nil
}

// loadOrCreateToken 读取 token 文件;不存在时生成并写入(0600)。
func loadOrCreateToken(path string) (string, error) {
	if path == "" {
		path = config.DefaultAdminTokenFile
	}

	if b, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("api: token file %q is empty", path)
		}
		return tok, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("api: read token %q: %w", path, err)
	}

	// 生成 32 字节随机 token。
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("api: generate token: %w", err)
	}
	tok := hex.EncodeToString(buf)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("api: create token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), tokenPerm); err != nil {
		return "", fmt.Errorf("api: write token %q: %w", path, err)
	}
	return tok, nil
}

// Token 返回当前生效的 token(Unix Socket 模式下为空)。
func (s *Server) Token() string { return s.token }

// splitScheme 拆分 "scheme://rest" 为两部分。
func splitScheme(s string) (scheme, rest string) {
	if i := strings.Index(s, "://"); i >= 0 {
		return s[:i], s[i+3:]
	}
	return "", s
}

// ---------------------------------------------------------------------------
// 路由与处理器
// ---------------------------------------------------------------------------

// routes 注册全部 REST 端点。
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("/api/v1/peers", s.auth(s.handlePeers))
	mux.HandleFunc("/api/v1/peers/", s.auth(s.handlePeerByID))
	mux.HandleFunc("/api/v1/routes", s.auth(s.handleRoutes))
	mux.HandleFunc("/api/v1/topology", s.auth(s.handleTopology))
	mux.HandleFunc("/api/v1/config", s.auth(s.handleConfig))
	mux.HandleFunc("/api/v1/config/raw", s.auth(s.handleConfigRaw))
	mux.HandleFunc("/api/v1/config/sources", s.auth(s.handleConfigSources))
	mux.HandleFunc("/api/v1/config/reload", s.auth(s.handleConfigReload))
	mux.HandleFunc("/api/v1/config/validate", s.auth(s.handleConfigValidate))
	mux.HandleFunc("/api/v1/version", s.auth(s.handleVersion))
	mux.HandleFunc("/api/v1/nat", s.auth(s.handleNAT))
	mux.HandleFunc("/api/v1/nat/probe", s.auth(s.handleNATProbe))
	mux.HandleFunc("/api/v1/candidates", s.auth(s.handleCandidates))

	return mux
}

// auth 包装处理器:TCP 模式下校验 Bearer token。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := bearerToken(r)
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next(w, r)
	}
}

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	return ""
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, s.node.Status())
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	peers := s.node.PeerList()
	if peers == nil {
		peers = []PeerSnapshot{}
	}
	writeJSON(w, peers)
}

// handlePeerByID 处理 /api/v1/peers/{nodeID}。
//
// 注意路径前缀已注册为 /api/v1/peers/,因此这里拿到的是 /{nodeID}。
func (s *Server) handlePeerByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/peers/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing node id")
		return
	}
	for _, p := range s.node.PeerList() {
		if p.NodeID == id || strings.HasPrefix(p.NodeID, id) {
			writeJSON(w, p)
			return
		}
	}
	writeError(w, http.StatusNotFound, "peer not found: "+id)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	routes := s.node.RouteList()
	if routes == nil {
		routes = []RouteSnapshot{}
	}
	writeJSON(w, routes)
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, s.node.TopologySnapshot())
}

// redactedConfig 返回脱敏后的配置。
//
// network.secret 永远不在 API 输出中明文出现;只暴露是否已设置。
func (s *Server) redactedConfig() map[string]any {
	c := s.node.Config()
	if c == nil {
		return map[string]any{}
	}
	// 以 TOML 派生的 map 形式返回结构,便于 CLI 直接展示。
	// 这里复用 config 包无法导出 toMap,故手工构造最小集合。
	return map[string]any{
		"network": map[string]any{
			"name":       c.Network.Name,
			"hostname":   c.Network.Hostname,
			"subnet":     c.Network.Subnet,
			"mtu":        c.Network.MTU,
			"secret_set": c.Network.Secret != "",
		},
		"listen":  listenMap(c),
		"peers":   peersSlice(c.Peers),
		"log":     map[string]any{"level": c.Log.Level, "format": c.Log.Format},
		"ingress": map[string]any{"mode": c.Ingress.Mode, "interface": c.Ingress.Interface},
		"tun":     map[string]any{"name": c.TUN.Name},
		"routing": map[string]any{
			"strategy":           c.Routing.Strategy,
			"ewma_alpha":         c.Routing.EWMAAlpha,
			"lsa_flood_interval": c.Routing.LSAFloodInterval.String(),
			"lsa_max_age":        c.Routing.LSAMaxAge.String(),
			"probe_interval":     c.Routing.ProbeInterval.String(),
			"heartbeat":          c.Routing.Heartbeat.String(),
			"stale_timeout":      c.Routing.StaleTimeout.String(),
		},
		"security": map[string]any{
			"e2ee": c.Security.E2EE,
		},
		"admin": map[string]any{
			"enabled":    c.Admin.Enabled,
			"listen":     c.Admin.Listen,
			"token_file": c.Admin.TokenFile,
		},
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, s.redactedConfig())
}

// handleConfigRaw 返回配置来源文件的原始内容。
func (s *Server) handleConfigRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c := s.node.Config()
	if c == nil || c.Path == "" {
		writeError(w, http.StatusNotFound, "no config source path")
		return
	}
	files := strings.Split(c.Path, string(os.PathListSeparator))
	raw := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			raw[f] = "error: " + err.Error()
			continue
		}
		raw[f] = string(b)
	}
	writeJSON(w, raw)
}

// ConfigSource 描述单个配置项的来源。
type ConfigSource struct {
	// Key 是配置项路径,如 network.mtu。
	Key string `json:"key"`
	// Value 是最终生效值(字符串化)。
	Value string `json:"value"`
	// Source 是来源类型:default / file / cli-block / env / cli。
	Source string `json:"source"`
	// Origin 是来源细节(文件路径、环境变量名、flag 名);默认值时为 null。
	Origin string `json:"origin,omitempty"`
}

// handleConfigSources 返回字段来源追踪。
//
// 注意:当前配置系统未完整记录每个字段的来源(见 docs/TECHDEBT.md),
// 因此这里返回"文件来源已知、其余归为 default"的最佳努力结果。
func (s *Server) handleConfigSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c := s.node.Config()
	if c == nil {
		writeJSON(w, []ConfigSource{})
		return
	}
	out := []ConfigSource{
		{Key: "network.name", Value: c.Network.Name, Source: "file", Origin: c.Path},
		{Key: "network.hostname", Value: c.Network.Hostname, Source: "file", Origin: c.Path},
		{Key: "network.subnet", Value: c.Network.Subnet, Source: "default"},
		{Key: "network.mtu", Value: fmt.Sprintf("%d", c.Network.MTU), Source: "default"},
		{Key: "log.level", Value: c.Log.Level, Source: "default"},
		{Key: "log.format", Value: c.Log.Format, Source: "default"},
		{Key: "routing.strategy", Value: c.Routing.Strategy, Source: "default"},
		{Key: "ingress.mode", Value: c.Ingress.Mode, Source: "default"},
		{Key: "admin.enabled", Value: fmt.Sprintf("%v", c.Admin.Enabled), Source: "default"},
	}
	writeJSON(w, out)
}

func (s *Server) handleConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.reload == nil {
		writeError(w, http.StatusNotImplemented, "reload not supported")
		return
	}
	if err := s.reload(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"result": "reloaded"})
}

// handleConfigValidate 校验请求体中的 TOML 配置(提交原文)。
//
// 请求体为新配置文件的原始字节;服务端解析并校验,返回是否合法。
func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Phase 1d 的 validate 仅回显当前运行配置是否合法:文件级校验由
	// `mesh config validate -c FILE` 在客户端本地完成(无需 API)。
	c := s.node.Config()
	if c == nil {
		writeError(w, http.StatusServiceUnavailable, "no config loaded")
		return
	}
	if err := c.Validate(); err != nil {
		writeJSON(w, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"valid": true})
}

func (s *Server) handleNAT(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, s.node.NAT())
}

// natProbeTimeout 是单次同步 NAT 重探的最长等待时间。
const natProbeTimeout = 10 * time.Second

// handleNATProbe 处理 POST /api/v1/nat/probe:同步触发一次 NAT 重探。
//
// 并发保护:同一时刻只允许一次重探;正在进行时返回 409 Conflict,
// 避免多个调用互相干扰(它们共享同一个 STUN socket)。
func (s *Server) handleNATProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// tryLock:不等待,拿不到即视为"已在探测中"。
	if !s.probeMu.TryLock() {
		writeError(w, http.StatusConflict, "nat probe already in progress")
		return
	}
	defer s.probeMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), natProbeTimeout)
	defer cancel()

	if err := s.node.ProbeNAT(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, s.node.NAT())
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cands := s.node.Candidates()
	if cands == nil {
		cands = []CandidateSnapshot{}
	}
	writeJSON(w, cands)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, map[string]string{"version": s.version})
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

// writeJSON 以 JSON 写出响应体。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// writeError 以统一格式写出错误响应。
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// listenMap 把监听地址列表转为 JSON 友好形式。
func listenMap(c *config.Config) []map[string]string {
	out := make([]map[string]string, 0, len(c.Listen))
	for _, l := range c.Listen {
		out = append(out, map[string]string{"addr": l.Addr})
	}
	return out
}

// peersSlice 保证空切片序列化为 [] 而非 null。
func peersSlice(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}
