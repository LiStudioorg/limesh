package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIngressDefaults 验证 [ingress] 与 [tun] 段的默认值符合 docs/CONFIG.md 6.4。
func TestIngressDefaults(t *testing.T) {
	cfg := Default()
	assert.Equal(t, "auto", cfg.Ingress.Mode)
	assert.Equal(t, "mesh0", cfg.Ingress.Interface)
	assert.Empty(t, cfg.TUN.Name, "tun.name 默认留空,回退到 ingress.interface")
	require.NoError(t, cfg.Validate())
}

// TestLoadIngressSection 验证从 TOML 读取 [ingress] 与 [tun] 段。
func TestLoadIngressSection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[ingress]
mode      = "tun"
interface = "mesh9"

[tun]
name = "mesh42"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, "tun", cfg.Ingress.Mode)
	assert.Equal(t, "mesh9", cfg.Ingress.Interface)
	assert.Equal(t, "mesh42", cfg.TUN.Name)
}

// TestEffectiveTUNName 覆盖接口名解析的优先级。
func TestEffectiveTUNName(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"默认", func(*Config) {}, "mesh0"},
		{"仅 ingress.interface", func(c *Config) { c.Ingress.Interface = "mesh1" }, "mesh1"},
		{"tun.name 优先", func(c *Config) {
			c.Ingress.Interface = "mesh1"
			c.TUN.Name = "custom0"
		}, "custom0"},
		{"ingress.interface 留空回退默认", func(c *Config) {
			c.Ingress.Interface = ""
		}, "mesh0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			assert.Equal(t, tt.want, cfg.EffectiveTUNName())
		})
	}
}

// TestValidateIngressMode 覆盖接入模式的合法与非法取值。
func TestValidateIngressMode(t *testing.T) {
	for _, mode := range []string{"tun", "no-tun", "auto"} {
		cfg := Default()
		cfg.Ingress.Mode = mode
		assert.NoError(t, cfg.Validate(), "mode=%q 应合法", mode)
	}

	cfg := Default()
	cfg.Ingress.Mode = "bogus"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ingress.mode")
}
