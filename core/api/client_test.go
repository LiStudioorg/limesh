package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/config"
)

// TestNewClientRequiresEndpoint 验证无端点时报错。
func TestNewClientRequiresEndpoint(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Listen = ""
	// Default() 的 EffectiveAdminListen 总有平台默认值,故显式用一个空配置。
	_, err := NewClient(ClientOptions{})
	require.ErrorIs(t, err, ErrNoAdminEndpoint)
	_ = cfg
}

// TestNewClientUnix 验证 Unix 客户端构造。
func TestNewClientUnix(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Admin.Listen = "unix://" + filepath.Join(dir, "x.sock")

	cl, err := NewClient(ClientOptions{Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, "http://unix", cl.BaseURL())
}

// TestNewClientTCPNoToken 验证 TCP 无 token 时报错。
func TestNewClientTCPNoToken(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Listen = "tcp://127.0.0.1:9090"
	cfg.Admin.TokenFile = filepath.Join(t.TempDir(), "missing.token")

	_, err := NewClient(ClientOptions{Config: cfg})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a token")
}

// TestNewClientTCPWithToken 验证显式 token 构造成功。
func TestNewClientTCPWithToken(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.Listen = "tcp://127.0.0.1:9090"
	cl, err := NewClient(ClientOptions{Config: cfg, Token: "abc"})
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9090", cl.BaseURL())
}

// TestNewClientTCPTokenFromFile 验证从文件读 token。
func TestNewClientTCPTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "admin.token")
	require.NoError(t, os.WriteFile(tf, []byte("filetoken\n"), 0o600))

	cfg := config.Default()
	cfg.Admin.Listen = "tcp://127.0.0.1:9091"
	cfg.Admin.TokenFile = tf

	cl, err := NewClient(ClientOptions{Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, "filetoken", cl.token)
}

// TestNewClientUnsupportedScheme 验证非法 scheme。
func TestNewClientUnsupportedScheme(t *testing.T) {
	_, err := NewClient(ClientOptions{Listen: "ftp://x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

// TestClientTimeout 验证超时处理。
func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	cl := &Client{base: srv.URL, hc: &http.Client{Timeout: 20 * time.Millisecond}}
	_, err := cl.Status(context.Background())
	require.Error(t, err)
}

// TestClientConnectionFailure 验证连接失败的错误信息。
func TestClientConnectionFailure(t *testing.T) {
	cl := &Client{base: "http://127.0.0.1:1", hc: &http.Client{Timeout: 100 * time.Millisecond}}
	_, err := cl.Status(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request")
}

// TestClientDecodeError 验证非法 JSON 响应。
func TestClientDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer srv.Close()

	cl := &Client{base: srv.URL, hc: srv.Client()}
	_, err := cl.Status(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// TestClientHTTPErrorStatus 验证非 2xx 被包装为错误。
func TestClientHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	cl := &Client{base: srv.URL, hc: srv.Client()}
	_, err := cl.Peers(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}
