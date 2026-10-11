package nat

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEncodeDecodeRoundTrip 验证候选编解码往返。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []Candidate{
		{Type: CandidateHost, Addr: netip.MustParseAddrPort("192.168.1.10:11010")},
		{Type: CandidateSrflx, Addr: netip.MustParseAddrPort("203.0.113.5:45231")},
		{Type: CandidateHost, Addr: netip.MustParseAddrPort("[2001:db8::1]:11010")},
	}

	buf := EncodeCandidates(in)
	out, err := DecodeCandidates(buf)
	require.NoError(t, err)
	require.Len(t, out, 3)

	assert.Equal(t, CandidateHost, out[0].Type)
	assert.Equal(t, "192.168.1.10:11010", out[0].Addr.String())
	assert.Equal(t, CandidateSrflx, out[1].Type)
	assert.Equal(t, "203.0.113.5:45231", out[1].Addr.String())
	assert.Equal(t, "[2001:db8::1]:11010", out[2].Addr.String())
}

// TestEncodeEmpty 验证空列表。
func TestEncodeEmpty(t *testing.T) {
	buf := EncodeCandidates(nil)
	assert.Equal(t, []byte{0}, buf)

	out, err := DecodeCandidates(buf)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// TestDecodeEmptyBuffer 验证空字节流返回 nil。
func TestDecodeEmptyBuffer(t *testing.T) {
	out, err := DecodeCandidates(nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// TestEncodeTruncatesToMax 验证超过上限被截断。
func TestEncodeTruncatesToMax(t *testing.T) {
	var in []Candidate
	for i := 0; i < MaxCandidates+5; i++ {
		in = append(in, Candidate{
			Type: CandidateHost,
			Addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 1)}), 11010),
		})
	}
	buf := EncodeCandidates(in)
	out, err := DecodeCandidates(buf)
	require.NoError(t, err)
	assert.Len(t, out, MaxCandidates)
}

// TestEncodeSkipsInvalidAddr 验证无效地址被跳过且计数正确。
func TestEncodeSkipsInvalidAddr(t *testing.T) {
	in := []Candidate{
		{Type: CandidateHost, Addr: netip.MustParseAddrPort("192.168.1.1:11010")},
		{Type: CandidateHost, Addr: netip.AddrPort{}}, // 无效
		{Type: CandidateHost, Addr: netip.MustParseAddrPort("192.168.1.2:11010")},
	}
	buf := EncodeCandidates(in)
	assert.Equal(t, byte(2), buf[0], "计数应为有效候选数")

	out, err := DecodeCandidates(buf)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "192.168.1.1:11010", out[0].Addr.String())
	assert.Equal(t, "192.168.1.2:11010", out[1].Addr.String())
}

// TestDecodeTruncated 验证截断字节流报错。
func TestDecodeTruncated(t *testing.T) {
	full := EncodeCandidates([]Candidate{
		{Type: CandidateHost, Addr: netip.MustParseAddrPort("192.168.1.1:11010")},
	})
	// 声明 2 条但只提供 1 条的数据。
	buf := append([]byte{2}, full[1:]...)

	_, err := DecodeCandidates(buf)
	require.ErrorIs(t, err, ErrCandidateDecode)
}

// TestDecodeBadFamily 验证非法地址族报错。
func TestDecodeBadFamily(t *testing.T) {
	// count=1, type=1, family=9, ...
	buf := []byte{1, 1, 9, 1, 2, 3, 4, 0, 50}
	_, err := DecodeCandidates(buf)
	require.ErrorIs(t, err, ErrCandidateDecode)
}

// TestTypeByteRoundTrip 验证类型字节转换。
func TestTypeByteRoundTrip(t *testing.T) {
	for _, ty := range []CandidateType{CandidateHost, CandidateSrflx, CandidateRelay} {
		assert.Equal(t, ty, typeFromByte(typeByte(ty)))
	}
	assert.Equal(t, CandidateType(""), typeFromByte(99))
	assert.Equal(t, byte(0), typeByte(CandidateType("bogus")))
}

// TestDecodeComputesPriority 验证解码后优先级被重新计算(非零)。
func TestDecodeComputesPriority(t *testing.T) {
	buf := EncodeCandidates([]Candidate{
		{Type: CandidateSrflx, Addr: netip.MustParseAddrPort("203.0.113.5:45231")},
	})
	out, err := DecodeCandidates(buf)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.NotZero(t, out[0].Priority)
}
