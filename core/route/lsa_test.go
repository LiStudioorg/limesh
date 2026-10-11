package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nid 构造一个确定性的 NodeID。
func nid(b byte) [NodeIDSize]byte {
	var id [NodeIDSize]byte
	for i := range id {
		id[i] = b + byte(i)
	}
	return id
}

// TestLSAMarshalRoundTrip 验证序列化往返一致。
func TestLSAMarshalRoundTrip(t *testing.T) {
	l := NewLSA(nid(1))
	l.SeqNo = 42
	l.Age = 7
	l.Flags = 0
	l.AddNeighbor(nid(2), 11)
	l.AddNeighbor(nid(3), 22)

	data, err := l.MarshalBinary()
	require.NoError(t, err)
	assert.Len(t, data, LSAHeaderSize+2*NeighborCostSize)

	var got LSA
	require.NoError(t, got.UnmarshalBinary(data))
	assert.Equal(t, l.Origin, got.Origin)
	assert.Equal(t, uint32(42), got.SeqNo)
	assert.Equal(t, uint16(7), got.Age)
	assert.Equal(t, uint16(0), got.Flags)
	require.Len(t, got.Neighbors, 2)
	assert.Equal(t, nid(2), got.Neighbors[0].NodeID)
	assert.Equal(t, uint32(11), got.Neighbors[0].Cost)
	assert.Equal(t, nid(3), got.Neighbors[1].NodeID)
	assert.Equal(t, uint32(22), got.Neighbors[1].Cost)
}

// TestLSAMarshalNoNeighbors 验证 0 邻居的边界。
func TestLSAMarshalNoNeighbors(t *testing.T) {
	l := NewLSA(nid(5))
	data, err := l.MarshalBinary()
	require.NoError(t, err)
	assert.Len(t, data, LSAHeaderSize)

	var got LSA
	require.NoError(t, got.UnmarshalBinary(data))
	assert.Empty(t, got.Neighbors)
}

// TestLSAMarshalManyNeighbors 验证大量邻居。
func TestLSAMarshalManyNeighbors(t *testing.T) {
	l := NewLSA(nid(1))
	const n = 1000
	for i := 0; i < n; i++ {
		l.AddNeighbor(nid(byte(i%200)+1), uint32(i))
	}

	data, err := l.MarshalBinary()
	require.NoError(t, err)

	var got LSA
	require.NoError(t, got.UnmarshalBinary(data))
	require.Len(t, got.Neighbors, n)
	assert.Equal(t, uint32(n-1), got.Neighbors[n-1].Cost)
}

// TestLSAMarshalNil 验证 nil 报错而非 panic。
func TestLSAMarshalNil(t *testing.T) {
	var l *LSA
	_, err := l.MarshalBinary()
	require.Error(t, err)
}

// TestLSAUnmarshalShort 验证过短输入报错。
func TestLSAUnmarshalShort(t *testing.T) {
	var l LSA
	err := l.UnmarshalBinary(make([]byte, LSAHeaderSize-1))
	require.ErrorIs(t, err, ErrShortLSA)
}

// TestLSAUnmarshalCountMismatch 验证邻居计数与字节数不符时报错。
func TestLSAUnmarshalCountMismatch(t *testing.T) {
	l := NewLSA(nid(1))
	l.AddNeighbor(nid(2), 1)
	data, err := l.MarshalBinary()
	require.NoError(t, err)

	// 声明 5 个邻居但只提供 1 个的数据。
	data[24], data[25] = 0, 5
	var got LSA
	err = got.UnmarshalBinary(data)
	require.ErrorIs(t, err, ErrBadNeighborCount)
}

// TestLSAClone 验证深拷贝不共享底层切片。
func TestLSAClone(t *testing.T) {
	l := NewLSA(nid(1))
	l.AddNeighbor(nid(2), 5)

	cp := l.Clone()
	cp.Neighbors[0].Cost = 99
	assert.Equal(t, uint32(5), l.Neighbors[0].Cost, "克隆不应影响原对象")
}

// TestLSAEqualIgnoresAge 验证 Equal 忽略 Age。
func TestLSAEqualIgnoresAge(t *testing.T) {
	a := NewLSA(nid(1))
	a.SeqNo = 3
	a.Age = 0
	a.AddNeighbor(nid(2), 5)

	b := a.Clone()
	b.Age = 100
	assert.True(t, a.Equal(b), "Age 不同不应判为不等")

	b.Neighbors[0].Cost = 6
	assert.False(t, a.Equal(b), "邻居开销不同应判为不等")
}

// TestLSANewerThan 覆盖序号比较与回绕。
func TestLSANewerThan(t *testing.T) {
	older := &LSA{SeqNo: 5}
	newer := &LSA{SeqNo: 6}
	assert.True(t, newer.NewerThan(older))
	assert.False(t, older.NewerThan(newer))
	assert.False(t, older.NewerThan(&LSA{SeqNo: 5}), "相同序号不更新")
	assert.True(t, newer.NewerThan(nil), "与 nil 比较视为更新")

	// 回绕:2^31-1 -> 2^31 视为更新(有符号差为正)。
	wrap := &LSA{SeqNo: 0x80000000}
	pre := &LSA{SeqNo: 0x7FFFFFFF}
	assert.True(t, wrap.NewerThan(pre))

	// 反向:从高位回绕到低位视为更旧。
	assert.False(t, pre.NewerThan(wrap))
}

// TestCostFunction 验证 cost = hop + RTT_ms。
func TestCostFunction(t *testing.T) {
	// 0 微秒:至少 1ms。
	assert.Equal(t, uint32(HopCost+1), Cost(0))
	// 1500 微秒 = 1.5ms -> 1ms。
	assert.Equal(t, uint32(HopCost+1), Cost(1500))
	// 5000 微秒 = 5ms。
	assert.Equal(t, uint32(HopCost+5), Cost(5000))
}
