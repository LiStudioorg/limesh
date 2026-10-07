package packet

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeaderSizeLayout(t *testing.T) {
	// 头部必须恰好 48 字节,与 AGENTS.md 8.3 节一致。
	assert.Equal(t, 48, HeaderSize)
	assert.Equal(t, 1+1+1+1+4+4+4+16+16, HeaderSize)
}

func TestNewSetsMagicAndVersion(t *testing.T) {
	p := New([]byte("hello"))

	assert.Equal(t, Magic, p.Magic())
	assert.Equal(t, ProtoVersion, p.Version())
	assert.Equal(t, byte('L'), p.Magic())
}

func TestNewWithHeadroomIsZeroCopy(t *testing.T) {
	// NewWithHeadroom 必须在调用方提供的数组上原地写头:共用底层数组。
	raw := make([]byte, HeaderSize+7)
	copy(raw[HeaderSize:], "payload")

	p, err := NewWithHeadroom(raw, 7)
	require.NoError(t, err)

	assert.Same(t, &raw[0], &p.Bytes()[0], "预期零拷贝(共用底层数组)")
	assert.Equal(t, []byte("payload"), p.Payload())
	assert.Equal(t, Magic, p.Magic())
}

func TestNewWithHeadroomRejectsShortBuffer(t *testing.T) {
	_, err := NewWithHeadroom(make([]byte, 10), 5)
	assert.ErrorIs(t, err, ErrShortBuffer)
}

func TestNewWithHeadroomRejectsBadLengths(t *testing.T) {
	_, err := NewWithHeadroom(make([]byte, 64), -1)
	assert.Error(t, err)

	_, err = NewWithHeadroom(make([]byte, MaxPayloadSize+HeaderSize+1), MaxPayloadSize+1)
	assert.ErrorIs(t, err, ErrPayloadTooLarge)
}

func TestNewCopiesPayload(t *testing.T) {
	// New 必须复制:修改原切片不得影响包内容。
	payload := []byte("no-room-here")
	p := New(payload)

	assert.Equal(t, payload, p.Payload())
	assert.NotSame(t, &payload[0], &p.Bytes()[0], "预期发生拷贝")

	payload[0] = 'X'
	assert.Equal(t, "no-room-here", string(p.Payload()))
}

func TestHeaderAndPayloadViews(t *testing.T) {
	p := New([]byte("abcdef"))

	assert.Len(t, p.Header(), HeaderSize)
	assert.Equal(t, []byte("abcdef"), p.Payload())
	assert.Equal(t, HeaderSize+6, p.Len())
	// Header 与 Payload 必须无重叠且紧邻。
	assert.Equal(t, HeaderSize, len(p.Bytes())-len(p.Payload()))
}

func TestFieldRoundTrip(t *testing.T) {
	p := New([]byte("body"))
	src := bytes.Repeat([]byte{0xAA}, NodeIDSize)
	dst := bytes.Repeat([]byte{0xBB}, NodeIDSize)

	p.SetType(TypeData)
	p.SetFlags(0)
	p.SetSessionID(0xDEADBEEF)
	p.SetSequence(42)
	p.SetAck(41)
	require.NoError(t, p.SetSrcNodeID(src))
	require.NoError(t, p.SetDstNodeID(dst))

	// 序列化后再解析,字段必须逐一还原。
	wire, err := p.Marshal()
	require.NoError(t, err)

	got, err := Unmarshal(wire)
	require.NoError(t, err)

	assert.Equal(t, TypeData, got.Type())
	assert.Equal(t, uint32(0xDEADBEEF), got.SessionID())
	assert.Equal(t, uint32(42), got.Sequence())
	assert.Equal(t, uint32(41), got.Ack())
	assert.Equal(t, src, got.SrcNodeID())
	assert.Equal(t, dst, got.DstNodeID())
	assert.Equal(t, []byte("body"), got.Payload())
}

func TestBigEndianEncoding(t *testing.T) {
	// 多字节字段必须是网络字节序(大端)。
	p := New(nil)
	p.SetSequence(0x01020304)

	seq := p.buf[offSequence : offSequence+4]
	assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, seq)
}

func TestSetNodeIDRejectsWrongLength(t *testing.T) {
	p := New(nil)

	assert.Error(t, p.SetSrcNodeID([]byte("short")))
	assert.Error(t, p.SetDstNodeID(make([]byte, NodeIDSize+1)))
	assert.NoError(t, p.SetSrcNodeID(make([]byte, NodeIDSize)))
}

func TestFlags(t *testing.T) {
	p := New(nil)

	assert.False(t, p.HasFlag(FlagEncrypted))
	p.SetFlag(FlagEncrypted)
	assert.True(t, p.HasFlag(FlagEncrypted))
	assert.True(t, p.HasFlag(FlagEncrypted|FlagRelayed), "任一位置位即命中")

	p.ClearFlag(FlagEncrypted)
	assert.False(t, p.HasFlag(FlagEncrypted))
}

func TestBoundaryEmptyPayload(t *testing.T) {
	p := New(nil)

	assert.Equal(t, HeaderSize, p.Len())
	assert.Empty(t, p.Payload())
	assert.NotNil(t, p.Payload(), "空载荷应返回非 nil 的零长切片")

	wire, err := p.Marshal()
	require.NoError(t, err)
	assert.Len(t, wire, HeaderSize)

	got, err := Unmarshal(wire)
	require.NoError(t, err)
	assert.Empty(t, got.Payload())
}

func TestBoundaryMaxPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5A}, MaxPayloadSize)
	p := New(payload)

	assert.Equal(t, MaxPayloadSize, len(p.Payload()))
	assert.Equal(t, HeaderSize+MaxPayloadSize, p.Len())

	wire, err := p.Marshal()
	require.NoError(t, err)

	got, err := Unmarshal(wire)
	require.NoError(t, err)
	assert.Equal(t, payload, got.Payload())
}

func TestUnmarshalRejectsShortBuffer(t *testing.T) {
	for _, n := range []int{0, 1, HeaderSize - 1} {
		_, err := Unmarshal(make([]byte, n))
		assert.ErrorIs(t, err, ErrShortBuffer, "长度 %d 应被拒绝", n)
	}
}

func TestUnmarshalRejectsBadMagic(t *testing.T) {
	wire := make([]byte, HeaderSize)
	wire[offMagic] = 0xFF

	_, err := Unmarshal(wire)
	assert.ErrorIs(t, err, ErrBadMagic)
}

func TestUnmarshalRejectsBadVersion(t *testing.T) {
	p := New(nil)
	p.SetVersion(99)

	_, err := Unmarshal(p.Bytes())
	assert.ErrorIs(t, err, ErrBadVersion)
}

func TestUnmarshalRejectsOversizePayload(t *testing.T) {
	wire := make([]byte, HeaderSize+MaxPayloadSize+1)
	wire[offMagic] = Magic
	wire[offVer] = ProtoVersion

	_, err := Unmarshal(wire)
	assert.ErrorIs(t, err, ErrPayloadTooLarge)
}

func TestUnmarshalIsZeroCopy(t *testing.T) {
	orig := New([]byte("shared"))
	wire, err := orig.Marshal()
	require.NoError(t, err)

	got, err := Unmarshal(wire)
	require.NoError(t, err)

	assert.Same(t, &wire[0], &got.Bytes()[0], "Unmarshal 不应复制")
}

func TestUnmarshalErrorsAreWrapped(t *testing.T) {
	// 错误必须可被 errors.Is 判定(用 %w 包装)。
	_, err := Unmarshal([]byte{0x00})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrShortBuffer))
	assert.Contains(t, err.Error(), "unmarshal", "错误应带上下文")
}

func TestNewWithCap(t *testing.T) {
	p, payload := NewWithCap(32)

	require.Len(t, payload, 32)
	assert.Equal(t, HeaderSize+32, p.Len())
	assert.Equal(t, Magic, p.Magic())

	// 写入载荷后应能直接读回(载荷区为定长,尾部未写部分保持零值)。
	copy(payload, "written-into-capacity")
	assert.Equal(t, "written-into-capacity", string(p.Payload()[:21]))
	assert.Len(t, p.Payload(), 32)
}

func TestNewWithCapClampsNegative(t *testing.T) {
	p, payload := NewWithCap(-5)
	assert.Empty(t, payload)
	assert.Equal(t, HeaderSize, p.Len())
}

func TestNewFromPoolAndRelease(t *testing.T) {
	p, payload := NewFromPool(16)
	require.Len(t, payload, 16)
	copy(payload, "pooled")

	assert.Equal(t, "pooled", string(p.Payload()[:6]))
	assert.Len(t, p.Payload(), 16)
	assert.Equal(t, Magic, p.Magic())

	p.Release()
	assert.Nil(t, p.Bytes(), "Release 后缓冲应被清空")

	// 重复 Release 必须安全(幂等)。
	assert.NotPanics(t, func() { p.Release() })
}

func TestReleaseOnNonPooledIsNoop(t *testing.T) {
	p := New([]byte("x"))
	assert.NotPanics(t, func() { p.Release() })
	assert.NotNil(t, p.Bytes(), "非池化包的 Release 不应改变自身")
}

func TestTypeName(t *testing.T) {
	assert.Equal(t, "handshake", TypeName(TypeHandshake))
	assert.Equal(t, "handshake-resp", TypeName(TypeHandshakeResp))
	assert.Equal(t, "data", TypeName(TypeData))
	assert.Equal(t, "keepalive", TypeName(TypeKeepalive))
	assert.Equal(t, "lsa", TypeName(TypeLSA))
	assert.Equal(t, "punch", TypeName(TypePunch))
	assert.Equal(t, "relay", TypeName(TypeRelay))
	assert.Equal(t, "probe", TypeName(TypeProbe))
	assert.Equal(t, "close", TypeName(TypeClose))
	assert.Contains(t, TypeName(200), "unknown")
}

func TestStringRedactsPayload(t *testing.T) {
	p := New([]byte("SECRET-MATERIAL"))
	p.SetType(TypeData)
	p.SetSequence(7)

	s := p.String()
	assert.Contains(t, s, "seq=7")
	assert.Contains(t, s, "payload=15")
	assert.NotContains(t, s, "SECRET-MATERIAL", "String 不得泄漏载荷内容")
}

func BenchmarkNew(b *testing.B) {
	payload := make([]byte, 1400)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = New(payload)
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	p := New(bytes.Repeat([]byte{0x01}, 1400))
	wire, _ := p.Marshal()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Unmarshal(wire); err != nil {
			b.Fatal(err)
		}
	}
}
