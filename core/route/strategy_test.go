package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewStrategy 覆盖策略构造。
func TestNewStrategy(t *testing.T) {
	for _, name := range []string{"", "shortest", "latency", "weighted"} {
		s, err := NewStrategy(name)
		require.NoError(t, err, "name=%q", name)
		assert.NotEmpty(t, s.Name())
	}

	_, err := NewStrategy("bogus")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownStrategy)
}

// TestShortestStrategy 验证 shortest 只计跳开销,忽略边权。
func TestShortestStrategy(t *testing.T) {
	s, _ := NewStrategy("shortest")
	assert.Equal(t, "shortest", s.Name())
	assert.Equal(t, uint32(HopCost), s.Weight(0, 0, 0))
	// 无论边权与丢包如何变化,shortest 的边权恒为 HopCost。
	assert.Equal(t, uint32(HopCost), s.Weight(5, 999999, 0.5), "忽略边权与丢包")
}

// TestLatencyStrategy 验证 latency 以边权为主并叠加丢包惩罚。
func TestLatencyStrategy(t *testing.T) {
	s, _ := NewStrategy("latency")
	assert.Equal(t, "latency", s.Name())

	low := s.Weight(1, 1000, 0)
	high := s.Weight(1, 50000, 0)
	assert.Less(t, low, high, "更高边权应产生更高开销")

	withLoss := s.Weight(1, 1000, 0.5)
	assert.Greater(t, withLoss, low, "丢包应增加开销")
}

// TestWeightedStrategy 验证 weighted 目前采用边权(接口预留)。
func TestWeightedStrategy(t *testing.T) {
	s, _ := NewStrategy("weighted")
	assert.Equal(t, "weighted", s.Name())
	assert.Equal(t, uint32(4000), s.Weight(3, 4000, 0.1))
}
