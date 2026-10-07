package config

import (
	"testing"
	"time"
)

// timeAfter 返回一个超时通道,用于给可能死循环的测试兜底。
func timeAfter(t *testing.T) <-chan time.Time {
	t.Helper()
	return time.After(5 * time.Second)
}
