package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNATDefaults 验证 [nat] 段默认值。
func TestNATDefaults(t *testing.T) {
	cfg := Default()
	assert.Equal(t, DefaultSTUNServers, cfg.NAT.STUNServers)
	assert.Equal(t, DefaultSTUNTimeout, cfg.NAT.STUNTimeout)
	assert.Equal(t, DefaultNATDetectCacheTTL, cfg.NAT.DetectCacheTTL)
	assert.Equal(t, DefaultCandidateSkipInterfaces, cfg.NAT.CandidateSkipInterfaces)
	require.NoError(t, cfg.Validate())
}

// TestLoadNATSection 验证从 TOML 读取 [nat] 段。
func TestLoadNATSection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[nat]
stun_servers              = ["stun:example.com:3478", "example.org:3479"]
stun_timeout              = "2s"
detect_cache_ttl          = "10m"
candidate_skip_interfaces = ["docker", "veth"]
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.Equal(t, []string{"stun:example.com:3478", "example.org:3479"}, cfg.NAT.STUNServers)
	assert.Equal(t, 2*time.Second, cfg.NAT.STUNTimeout)
	assert.Equal(t, 10*time.Minute, cfg.NAT.DetectCacheTTL)
	assert.Equal(t, []string{"docker", "veth"}, cfg.NAT.CandidateSkipInterfaces)
}

// TestValidateNAT 覆盖 [nat] 校验规则。
func TestValidateNAT(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"默认", func(*Config) {}, false},
		{"stun_timeout 为 0", func(c *Config) { c.NAT.STUNTimeout = 0 }, true},
		{"stun_timeout 为负", func(c *Config) { c.NAT.STUNTimeout = -time.Second }, true},
		{"cache_ttl 为负", func(c *Config) { c.NAT.DetectCacheTTL = -time.Second }, true},
		{"cache_ttl 为 0", func(c *Config) { c.NAT.DetectCacheTTL = 0 }, false},
		{"stun 空项", func(c *Config) { c.NAT.STUNServers = []string{""} }, true},
		{"stun 无端口", func(c *Config) { c.NAT.STUNServers = []string{"stun:example.com"} }, true},
		{"stun 端口非法", func(c *Config) { c.NAT.STUNServers = []string{"example.com:99999"} }, true},
		{"stun 合法带前缀", func(c *Config) { c.NAT.STUNServers = []string{"stun:example.com:3478"} }, false},
		{"stun 合法裸主机端口", func(c *Config) { c.NAT.STUNServers = []string{"example.com:3478"} }, false},
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

// TestNATSnapshotDeepCopy 验证快照深拷贝 NAT 切片。
func TestNATSnapshotDeepCopy(t *testing.T) {
	orig := Default()
	snap := orig.Snapshot()

	orig.NAT.STUNServers[0] = "mutated"
	orig.NAT.CandidateSkipInterfaces[0] = "mutated"

	assert.NotEqual(t, "mutated", snap.NAT.STUNServers[0])
	assert.NotEqual(t, "mutated", snap.NAT.CandidateSkipInterfaces[0])
}
