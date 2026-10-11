package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LiStudioorg/limesh/core/config"
)

// Client 相关错误。
var (
	// ErrNoAdminEndpoint 表示未配置可用的 Admin 端点。
	ErrNoAdminEndpoint = errors.New("api: no admin endpoint configured")
)

// Client 是 Admin API 的 HTTP 客户端。
//
// 支持 Unix Socket 与 TCP 两种传输;TCP 模式下自动附带 Bearer token。
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// ClientOptions 是构造 Client 的参数。
type ClientOptions struct {
	// Config 是配置快照,用于推导默认端点与 token 文件路径。
	Config *config.Config
	// Listen 覆盖监听地址(unix:// 或 tcp://);为空时用 Config 推导。
	Listen string
	// Token 覆盖 token;为空且为 TCP 时从 TokenFile 读取。
	Token string
	// Timeout 是请求超时;为 0 时使用 10 秒。
	Timeout time.Duration
}

// NewClient 依据配置构造客户端。
//
// 端点推导顺序:opts.Listen > Config.EffectiveAdminListen()。
// TCP 模式下 token 推导:opts.Token > TokenFile 内容。
func NewClient(opts ClientOptions) (*Client, error) {
	listen := opts.Listen
	if listen == "" && opts.Config != nil {
		listen = opts.Config.EffectiveAdminListen()
	}
	if listen == "" {
		return nil, ErrNoAdminEndpoint
	}

	scheme, addr := splitScheme(listen)
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	c := &Client{}

	switch scheme {
	case "unix":
		// Unix Socket:用自定义 DialContext 连接 socket,URL 用占位 host。
		socket := addr
		c.base = "http://unix"
		c.hc = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		}

	case "tcp":
		tok := opts.Token
		if tok == "" {
			tok = readTokenFile(tokenFileOf(opts.Config))
		}
		if tok == "" {
			return nil, fmt.Errorf("api: tcp endpoint %q requires a token", addr)
		}
		c.token = tok
		c.base = "http://" + addr
		c.hc = &http.Client{Timeout: timeout}

	default:
		return nil, fmt.Errorf("api: unsupported listen scheme %q", scheme)
	}

	return c, nil
}

// tokenFileOf 返回配置中的 token 文件路径,为空时用默认值。
func tokenFileOf(cfg *config.Config) string {
	if cfg != nil && cfg.Admin.TokenFile != "" {
		return cfg.Admin.TokenFile
	}
	return config.DefaultAdminTokenFile
}

// readTokenFile 读取 token 文件,失败返回空串。
func readTokenFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// BaseURL 返回客户端使用的基地址(便于排错)。
func (c *Client) BaseURL() string { return c.base }

// Status 获取节点状态。
func (c *Client) Status(ctx context.Context) (*StatusSnapshot, error) {
	var out StatusSnapshot
	if err := c.get(ctx, "/api/v1/status", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Peers 获取全部对端。
func (c *Client) Peers(ctx context.Context) ([]PeerSnapshot, error) {
	var out []PeerSnapshot
	if err := c.get(ctx, "/api/v1/peers", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Peer 获取单个对端;id 可以是完整或前缀 NodeID。
func (c *Client) Peer(ctx context.Context, id string) (*PeerSnapshot, error) {
	var out PeerSnapshot
	if err := c.get(ctx, "/api/v1/peers/"+id, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Routes 获取路由表。
func (c *Client) Routes(ctx context.Context) ([]RouteSnapshot, error) {
	var out []RouteSnapshot
	if err := c.get(ctx, "/api/v1/routes", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Topology 获取拓扑。
func (c *Client) Topology(ctx context.Context) (*TopologySnapshot, error) {
	var out TopologySnapshot
	if err := c.get(ctx, "/api/v1/topology", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Config 获取脱敏后的配置。
func (c *Client) Config(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.get(ctx, "/api/v1/config", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfigRaw 获取原始配置文件内容(路径 -> 内容)。
func (c *Client) ConfigRaw(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	if err := c.get(ctx, "/api/v1/config/raw", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ConfigSources 获取字段来源追踪。
func (c *Client) ConfigSources(ctx context.Context) ([]ConfigSource, error) {
	var out []ConfigSource
	if err := c.get(ctx, "/api/v1/config/sources", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// NAT 获取 NAT 穿透观测。
func (c *Client) NAT(ctx context.Context) (*NATSnapshot, error) {
	var out NATSnapshot
	if err := c.get(ctx, "/api/v1/nat", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProbeNAT 触发一次同步 NAT 重探,返回刷新后的观测结果。
//
// 服务端在重探进行中会返回 409;客户端原样把它包装为错误。
func (c *Client) ProbeNAT(ctx context.Context) (*NATSnapshot, error) {
	var out NATSnapshot
	if err := c.post(ctx, "/api/v1/nat/probe", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Candidates 获取候选地址列表。
func (c *Client) Candidates(ctx context.Context) ([]CandidateSnapshot, error) {
	var out []CandidateSnapshot
	if err := c.get(ctx, "/api/v1/candidates", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Version 获取节点版本。
func (c *Client) Version(ctx context.Context) (string, error) {
	var out map[string]string
	if err := c.get(ctx, "/api/v1/version", &out); err != nil {
		return "", err
	}
	return out["version"], nil
}

// ReloadConfig 触发配置热重载。
func (c *Client) ReloadConfig(ctx context.Context) error {
	return c.post(ctx, "/api/v1/config/reload", nil, nil)
}

// get 执行 GET 并解析 JSON 到 out。
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("api: build request %s: %w", path, err)
	}
	return c.do(req, out)
}

// post 执行 POST。
func (c *Client) post(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("api: build request %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// do 发送请求并处理响应。
//
// 非 2xx 一律返回带状态码与响应体的错误,便于 CLI 展示。
func (c *Client) do(req *http.Request, out any) error {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("api: request %s: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("api: read response %s: %w", req.URL.Path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("api: %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("api: decode response %s: %w", req.URL.Path, err)
	}
	return nil
}
