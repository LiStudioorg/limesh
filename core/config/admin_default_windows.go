//go:build windows

package config

// DefaultAdminListen 返回 Windows 平台默认的 Admin API 监听地址。
//
// Windows 无 Unix Socket,退化为本机回环 TCP,并强制启用 token 认证。
func DefaultAdminListen() string {
	return "tcp://" + DefaultAdminTCPListen
}
