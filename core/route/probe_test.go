package route

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSender 记录发送的探测与确认。
type fakeSender struct {
	mu      sync.Mutex
	probes  [][2]any // (peerID, payload)
	acks    [][2]any
	probeEr error
}

func (f *fakeSender) SendProbe(peerID [NodeIDSize]byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, [2]any{peerID, payload})
	return f.probeEr
}

func (f *fakeSender) SendProbeAck(peerID [NodeIDSize]byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, [2]any{peerID, payload})
	return nil
}

func (f *fakeSender) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.probes)
}

func (f *fakeSender) ackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.acks)
}

// TestProbeEncodeDecodeRoundTrip 验证探测载荷编解码。
func TestProbeEncodeDecodeRoundTrip(t *testing.T) {
	now := time.Unix(0, 1234567890)
	payload := EncodeProbe(7, now)
	assert.Len(t, payload, ProbePayloadSize)

	seq, send, err := DecodeProbe(payload)
	require.NoError(t, err)
	assert.Equal(t, uint32(7), seq)
	assert.Equal(t, uint64(now.UnixNano()), send)
}

// TestProbeDecodeShort 验证过短探测载荷报错。
func TestProbeDecodeShort(t *testing.T) {
	_, _, err := DecodeProbe(make([]byte, 4))
	require.ErrorIs(t, err, ErrShortProbe)
}

// TestProbeAckEncodeDecodeRoundTrip 验证确认载荷编解码。
func TestProbeAckEncodeDecodeRoundTrip(t *testing.T) {
	recv := time.Unix(0, 999)
	payload := EncodeProbeAck(3, 888, recv)
	seq, echo, err := DecodeProbeAck(payload)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), seq)
	assert.Equal(t, uint64(888), echo)
}

// TestProbeAckDecodeShort 验证过短确认载荷报错。
func TestProbeAckDecodeShort(t *testing.T) {
	_, _, err := DecodeProbeAck(make([]byte, 4))
	require.ErrorIs(t, err, ErrShortProbe)
}

// TestNewProberRejectsNilSender 验证 nil sender 被拒绝。
func TestNewProberRejectsNilSender(t *testing.T) {
	_, err := NewProber(ProberOptions{})
	require.Error(t, err)
}

// TestProberRegisterUnregister 验证注册与移除。
func TestProberRegisterUnregister(t *testing.T) {
	p, err := NewProber(ProberOptions{Sender: &fakeSender{}})
	require.NoError(t, err)

	p.Register(nid(1))
	p.Register(nid(2))
	assert.Len(t, p.Peers(), 2)

	p.Unregister(nid(1))
	assert.Len(t, p.Peers(), 1)
}

// TestProberHandleProbeSendsAck 验证收到探测后回送确认。
func TestProberHandleProbeSendsAck(t *testing.T) {
	sender := &fakeSender{}
	p, err := NewProber(ProberOptions{Sender: sender})
	require.NoError(t, err)

	now := time.Unix(0, 5000)
	payload := EncodeProbe(1, now)
	require.NoError(t, p.HandleProbe(nid(2), payload, now.Add(time.Millisecond)))

	assert.Equal(t, 1, sender.ackCount())
}

// TestProberHandleProbeBadPayload 验证非法载荷报错。
func TestProberHandleProbeBadPayload(t *testing.T) {
	p, err := NewProber(ProberOptions{Sender: &fakeSender{}})
	require.NoError(t, err)
	err = p.HandleProbe(nid(2), []byte{1, 2}, time.Now())
	require.Error(t, err)
}

// TestProberEWMASmoothing 验证 RTT 的 EWMA 平滑。
func TestProberEWMASmoothing(t *testing.T) {
	sender := &fakeSender{}
	p, err := NewProber(ProberOptions{Sender: sender, EWMAAlpha: 0.5})
	require.NoError(t, err)

	base := time.Unix(0, 0)
	send := uint64(base.UnixNano())

	// 第一个样本 100ms。
	ack1 := EncodeProbeAck(1, send, base)
	require.NoError(t, p.HandleProbeAck(nid(2), ack1, base.Add(100*time.Millisecond)))
	st := p.Stats(nid(2))
	assert.InDelta(t, 100*time.Millisecond, st.RTT, float64(time.Millisecond))

	// 第二个样本 200ms:alpha=0.5 -> EWMA = 150ms。
	ack2 := EncodeProbeAck(2, send, base)
	require.NoError(t, p.HandleProbeAck(nid(2), ack2, base.Add(200*time.Millisecond)))
	st = p.Stats(nid(2))
	assert.InDelta(t, 150*time.Millisecond, st.RTT, float64(time.Millisecond))
}

// TestProberLossRate 验证丢包率估算。
func TestProberLossRate(t *testing.T) {
	sender := &fakeSender{}
	p, err := NewProber(ProberOptions{Sender: sender, Interval: time.Millisecond})
	require.NoError(t, err)
	p.Register(nid(2))

	// 发送若干探测但只确认一半。
	for i := 0; i < 10; i++ {
		p.mu.Lock()
		p.seq++
		seq := p.seq
		p.mu.Unlock()
		st := p.stats[nid(2)]
		st.sent++
		st.outstanding++
		_ = seq
	}
	for i := 0; i < 5; i++ {
		ack := EncodeProbeAck(uint32(i), 0, time.Now())
		require.NoError(t, p.HandleProbeAck(nid(2), ack, time.Now()))
	}

	st := p.Stats(nid(2))
	assert.Greater(t, st.LossRate, 0.0, "有未确认探测时应报告丢包")
	assert.LessOrEqual(t, st.LossRate, 1.0)
}

// TestProberRunStopsOnContext 验证 Run 在 ctx 取消后退出。
func TestProberRunStopsOnContext(t *testing.T) {
	sender := &fakeSender{}
	p, err := NewProber(ProberOptions{Sender: sender, Interval: time.Millisecond})
	require.NoError(t, err)
	p.Register(nid(2))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	require.Eventually(t, func() bool { return sender.probeCount() > 0 },
		time.Second, 2*time.Millisecond, "应周期性发送探测")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在取消后退出")
	}
}

// TestProberStatsUnknownPeer 验证未注册对端返回零值。
func TestProberStatsUnknownPeer(t *testing.T) {
	p, err := NewProber(ProberOptions{Sender: &fakeSender{}})
	require.NoError(t, err)
	st := p.Stats(nid(200))
	assert.Equal(t, time.Duration(0), st.RTT)
	assert.Equal(t, float64(0), st.LossRate)
}

// TestProberSendErrorDoesNotPanic 验证发送错误被容忍。
func TestProberSendErrorDoesNotPanic(t *testing.T) {
	sender := &fakeSender{probeEr: errors.New("net down")}
	p, err := NewProber(ProberOptions{Sender: sender, Interval: time.Millisecond})
	require.NoError(t, err)
	p.Register(nid(2))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.NotPanics(t, func() { p.Run(ctx) })
}
