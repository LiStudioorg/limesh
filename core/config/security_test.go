package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurityDefaults 验证 [security] 段的默认值符合 docs/CONFIG.md 6.8。
func TestSecurityDefaults(t *testing.T) {
	cfg := Default()
	s := cfg.Security

	assert.True(t, s.E2EE, "security.e2ee 默认应为 true")
	assert.Equal(t, 168*time.Hour, s.KeyRotation.Interval)
	assert.Equal(t, 24*time.Hour, s.KeyRotation.Overlap)
	assert.Equal(t, "allow", s.ACL.Default)
	assert.Empty(t, s.ACL.Rules)
	assert.False(t, s.Audit.Enabled)
	assert.Equal(t, "/var/lib/mesh/audit.log", s.Audit.Path)
	require.NoError(t, cfg.Validate())
}

// TestLoadSecuritySection 验证从 TOML 读取 [security] 段的完整路径,
// 包括 duration 字符串解析与嵌套 ACL 规则。
func TestLoadSecuritySection(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", `
[security]
e2ee = false

[security.key_rotation]
interval = "48h"
overlap  = "6h"

[security.acl]
default = "deny"

[[security.acl.rules]]
action = "allow"
from   = "10.126.126.0/24"
to     = "10.126.126.42"
ports  = [5432, 6379]
proto  = "tcp"

[[security.acl.rules]]
action = "deny"
from   = "10.126.126.9"
proto  = "any"

[security.audit]
enabled = true
path    = "/var/log/mesh/audit.log"
`)

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)

	s := cfg.Security
	assert.False(t, s.E2EE)
	assert.Equal(t, 48*time.Hour, s.KeyRotation.Interval)
	assert.Equal(t, 6*time.Hour, s.KeyRotation.Overlap)
	assert.Equal(t, "deny", s.ACL.Default)
	require.Len(t, s.ACL.Rules, 2)

	assert.Equal(t, ACLRule{
		Action: "allow",
		From:   "10.126.126.0/24",
		To:     "10.126.126.42",
		Ports:  []int{5432, 6379},
		Proto:  "tcp",
	}, s.ACL.Rules[0])

	assert.Equal(t, ACLRule{
		Action: "deny",
		From:   "10.126.126.9",
		Proto:  "any",
	}, s.ACL.Rules[1])

	assert.True(t, s.Audit.Enabled)
	assert.Equal(t, "/var/log/mesh/audit.log", s.Audit.Path)
}

// TestLoadSecurityACLRulesAppend 验证多文件合并时 ACL 规则按追加语义累积,
// 与配置系统"数组追加"的全局规则一致。
func TestLoadSecurityACLRulesAppend(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	base := writeFile(t, dir, "base.toml", `
[[security.acl.rules]]
action = "allow"
from   = "10.126.126.0/24"
`)
	over := writeFile(t, dir, "override.toml", `
[[security.acl.rules]]
action = "deny"
from   = "10.126.126.7"
`)

	cfg, err := Load(Options{Paths: []string{base, over}})
	require.NoError(t, err)

	require.Len(t, cfg.Security.ACL.Rules, 2)
	assert.Equal(t, "allow", cfg.Security.ACL.Rules[0].Action)
	assert.Equal(t, "deny", cfg.Security.ACL.Rules[1].Action)
}

// TestLoadSecurityEnvOverride 验证环境变量可以覆盖 [security] 段,
// 并确认 MESH_SECURITY_E2EE 的路径映射生效。
func TestLoadSecurityEnvOverride(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := writeFile(t, dir, "config.toml", "[security]\ne2ee = true\n")

	t.Setenv("MESH_SECURITY_E2EE", "false")

	cfg, err := Load(Options{Paths: []string{p}})
	require.NoError(t, err)
	assert.False(t, cfg.Security.E2EE, "环境变量应覆盖文件字段")
}

// TestValidateSecurityKeyRotation 覆盖密钥轮换周期的边界与越界。
func TestValidateSecurityKeyRotation(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		overlap  time.Duration
		wantErr  bool
	}{
		{"默认", 168 * time.Hour, 24 * time.Hour, false},
		{"interval 下限", MinKeyRotationInterval, 0, false},
		{"interval 上限", MaxKeyRotationInterval, 24 * time.Hour, false},
		{"interval 过小", MinKeyRotationInterval - time.Second, 0, true},
		{"interval 过大", MaxKeyRotationInterval + time.Second, 0, true},
		{"overlap 为 0", 168 * time.Hour, 0, false},
		{"overlap 上限", 720 * time.Hour * 2, MaxKeyRotationOverlap, false},
		{"overlap 为负", 168 * time.Hour, -time.Second, true},
		{"overlap 超上限", 8760 * time.Hour, MaxKeyRotationOverlap + time.Second, true},
		{"overlap 超过 interval", 2 * time.Hour, 10 * time.Hour, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Security.KeyRotation.Interval = tt.interval
			cfg.Security.KeyRotation.Overlap = tt.overlap

			err := cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestValidateSecurityACL 覆盖 ACL 默认动作与规则的校验。
func TestValidateSecurityACL(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"默认动作 allow", func(c *Config) { c.Security.ACL.Default = "allow" }, false},
		{"默认动作 deny", func(c *Config) { c.Security.ACL.Default = "deny" }, false},
		{"默认动作非法", func(c *Config) { c.Security.ACL.Default = "reject" }, true},
		{"规则动作合法", func(c *Config) {
			c.Security.ACL.Rules = []ACLRule{{Action: "allow"}}
		}, false},
		{"规则动作非法", func(c *Config) {
			c.Security.ACL.Rules = []ACLRule{{Action: "drop"}}
		}, true},
		{"端口越界", func(c *Config) {
			c.Security.ACL.Rules = []ACLRule{{Action: "allow", Ports: []int{70000}}}
		}, true},
		{"端口为负", func(c *Config) {
			c.Security.ACL.Rules = []ACLRule{{Action: "allow", Ports: []int{-1}}}
		}, true},
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

// TestValidateSecurityAuditPath 验证启用审计但路径为空时被拒绝。
func TestValidateSecurityAuditPath(t *testing.T) {
	cfg := Default()
	cfg.Security.Audit.Enabled = true
	cfg.Security.Audit.Path = ""
	assert.Error(t, cfg.Validate())

	cfg.Security.Audit.Path = "/tmp/audit.log"
	assert.NoError(t, cfg.Validate())

	// 未启用时路径为空也应通过。
	cfg.Security.Audit.Enabled = false
	cfg.Security.Audit.Path = ""
	assert.NoError(t, cfg.Validate())
}

// TestSnapshotDeepCopiesSecurityRules 守护快照的深拷贝语义:
// 快照与原件不得共享 ACL 规则切片,否则热重载会出现隐性别名。
func TestSnapshotDeepCopiesSecurityRules(t *testing.T) {
	orig := Default()
	orig.Security.ACL.Rules = []ACLRule{
		{Action: "allow", Ports: []int{22, 80}},
	}

	snap := orig.Snapshot()

	// 修改原始配置,快照应保持不受影响。
	orig.Security.ACL.Rules[0].Action = "deny"
	orig.Security.ACL.Rules[0].Ports[0] = 9999

	assert.Equal(t, "allow", snap.Security.ACL.Rules[0].Action)
	assert.Equal(t, []int{22, 80}, snap.Security.ACL.Rules[0].Ports)
}
