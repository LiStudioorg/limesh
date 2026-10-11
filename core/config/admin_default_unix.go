//go:build !windows

package config

// DefaultAdminListen 返回 Unix 平台默认的 Admin API 监听地址。
//
// Linux/macOS 使用 Unix Socket:天然只允许本机访问,且可用文件权限 0600
// 限制到运行用户,无需额外的 token。
func DefaultAdminListen() string {
	return "unix://" + DefaultAdminUnixSocket
}
