package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminDefaults 验证 [admin] 段默认值。
func TestAdminDefaults(t *testing.T) {
	cfg := Default()
	assert.False(t, cfg.Admin.Enabled)
	assert.Empty(t, cfg.Admin.Listen)
	assert.Equal(t, DefaultAdminTokenFile, cfg.Admin.TokenFile)
	require.NoError(t, cfg.Validate())
}

// TestLoadAdminSection 验证从 TOML 读取 [admin] 段。
func TestLoadAdminSection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[admin]
enabled    = true
listen     = "tcp://127.0.0.1:9200"
token_file = "/tmp/tok"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.True(t, cfg.Admin.Enabled)
	assert.Equal(t, "tcp://127.0.0.1:9200", cfg.Admin.Listen)
	assert.Equal(t, "/tmp/tok", cfg.Admin.TokenFile)
}

// TestEffectiveAdminListen 验证默认监听地址解析。
func TestEffectiveAdminListen(t *testing.T) {
	cfg := Default()
	// 未显式配置时使用平台默认值。
	assert.NotEmpty(t, cfg.EffectiveAdminListen())

	cfg.Admin.Listen = "unix:///custom.sock"
	assert.Equal(t, "unix:///custom.sock", cfg.EffectiveAdminListen())
}

// TestValidateAdmin 覆盖 [admin] 校验。
func TestValidateAdmin(t *testing.T) {
	tests := []struct {
		name    string
		listen  string
		wantErr bool
	}{
		{"空(用默认)", "", false},
		{"unix 合法", "unix:///tmp/a.sock", false},
		{"unix 空路径", "unix://", true},
		{"tcp 合法", "tcp://127.0.0.1:9090", false},
		{"tcp 非法", "tcp://not-an-addr", true},
		{"非法 scheme", "ftp://x", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Admin.Listen = tt.listen
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
