package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoutingDefaults 验证 [routing] 段的默认值。
func TestRoutingDefaults(t *testing.T) {
	cfg := Default()
	assert.Equal(t, "shortest", cfg.Routing.Strategy)
	assert.Equal(t, 0.2, cfg.Routing.EWMAAlpha)
	assert.Equal(t, 30*time.Second, cfg.Routing.LSAFloodInterval)
	assert.Equal(t, 300*time.Second, cfg.Routing.LSAMaxAge)
	assert.Equal(t, time.Second, cfg.Routing.ProbeInterval)
	assert.Equal(t, 5*time.Second, cfg.Routing.Heartbeat)
	assert.Equal(t, 20*time.Second, cfg.Routing.StaleTimeout)
	require.NoError(t, cfg.Validate())
}

// TestLoadRoutingSection 验证从 TOML 读取 [routing] 段。
func TestLoadRoutingSection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[routing]
strategy           = "latency"
ewma_alpha         = 0.3
lsa_flood_interval = "15s"
lsa_max_age        = "120s"
probe_interval     = "2s"
heartbeat          = "4s"
stale_timeout      = "10s"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, "latency", cfg.Routing.Strategy)
	assert.Equal(t, 0.3, cfg.Routing.EWMAAlpha)
	assert.Equal(t, 15*time.Second, cfg.Routing.LSAFloodInterval)
	assert.Equal(t, 120*time.Second, cfg.Routing.LSAMaxAge)
	assert.Equal(t, 2*time.Second, cfg.Routing.ProbeInterval)
	assert.Equal(t, 4*time.Second, cfg.Routing.Heartbeat)
	assert.Equal(t, 10*time.Second, cfg.Routing.StaleTimeout)
}

// TestValidateRouting 覆盖 [routing] 的校验规则。
func TestValidateRouting(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"默认", func(*Config) {}, false},
		{"策略非法", func(c *Config) { c.Routing.Strategy = "bogus" }, true},
		{"ewma 为 0", func(c *Config) { c.Routing.EWMAAlpha = 0 }, true},
		{"ewma 大于 1", func(c *Config) { c.Routing.EWMAAlpha = 1.5 }, true},
		{"ewma 等于 1", func(c *Config) { c.Routing.EWMAAlpha = 1 }, false},
		{"泛洪周期为 0", func(c *Config) { c.Routing.LSAFloodInterval = 0 }, true},
		{"max_age 为 0", func(c *Config) { c.Routing.LSAMaxAge = 0 }, true},
		{"max_age 小于泛洪周期", func(c *Config) {
			c.Routing.LSAFloodInterval = 30 * time.Second
			c.Routing.LSAMaxAge = 10 * time.Second
		}, true},
		{"探测周期为 0", func(c *Config) { c.Routing.ProbeInterval = 0 }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
