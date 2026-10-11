package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewReplayWindowValidation(t *testing.T) {
	_, err := NewReplayWindow(0)
	require.Error(t, err, "0 位窗口应被拒绝")

	_, err = NewReplayWindow(-1)
	require.Error(t, err)

	_, err = NewReplayWindow(100)
	require.Error(t, err, "非 64 倍数应被拒绝")

	_, err = NewReplayWindow(2 << 20)
	require.Error(t, err, "超过上界应被拒绝")

	w, err := NewReplayWindow(512)
	require.NoError(t, err)
	assert.Equal(t, 512, w.Size())
}

func TestMustReplayWindowPanicsOnBadSize(t *testing.T) {
	assert.Panics(t, func() { MustReplayWindow(0) })
	assert.NotPanics(t, func() { MustReplayWindow(64) })
}

func TestReplayWindowAcceptsFirstAnySequence(t *testing.T) {
	// 首个包可能携带大序号(对端重启、长静默),必须被接受,
	// 否则会话永远无法开始。
	for _, seq := range []uint64{0, 1, 42, 1 << 20} {
		w := MustReplayWindow(DefaultWindowSize)
		assert.True(t, w.Accept(seq), "首个序号 %d 应被接受", seq)
		assert.False(t, w.Initialized() == false)
		assert.Equal(t, seq, w.Max())
	}
}

func TestReplayWindowSequentialAccepted(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)

	for seq := uint64(0); seq < 100; seq++ {
		require.True(t, w.Accept(seq), "序号 %d 应被接受", seq)
	}
	assert.Equal(t, uint64(99), w.Max())
}

func TestReplayWindowRejectsDuplicate(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)

	require.True(t, w.Accept(5))
	assert.False(t, w.Accept(5), "重复序号必须被拒绝")
	assert.False(t, w.Accept(5), "重复拒绝应是幂等的")
}

func TestReplayWindowAcceptsWithinWindowOutOfOrder(t *testing.T) {
	// 乱序但在窗口内:必须接受(多路径并行会造成乱序)。
	w := MustReplayWindow(DefaultWindowSize)

	require.True(t, w.Accept(100))

	for _, seq := range []uint64{99, 98, 50, 1, 0} {
		assert.True(t, w.Accept(seq), "窗口内的乱序序号 %d 应被接受", seq)
	}
	assert.Equal(t, uint64(100), w.Max(), "乱序不应推进 max")
}

func TestReplayWindowRejectsTooOld(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)

	require.True(t, w.Accept(DefaultWindowSize+10))

	// 落后超过一个窗口宽度的序号一律拒绝:已无法判断是否重放。
	assert.False(t, w.Accept(0), "过老序号应被拒绝")
	assert.False(t, w.Accept(5), "过老序号应被拒绝")

	// 恰好落在窗口边界内的仍应接受。
	boundary := uint64(DefaultWindowSize + 10 - DefaultWindowSize + 1)
	assert.True(t, w.Accept(boundary), "窗口边界内的序号应被接受")
}

func TestReplayWindowRejectsExcessiveJump(t *testing.T) {
	// 单次前跳超过一个窗口宽度会掏空整个位图,使所有在途包变成"过老"。
	// 这类序号必须被拒绝。
	w := MustReplayWindow(DefaultWindowSize)
	require.True(t, w.Accept(0))

	assert.False(t, w.Accept(1<<40), "超大跳跃应被拒绝")

	// 跳跃被拒绝后,窗口状态不应被破坏。
	assert.True(t, w.Accept(1), "正常的后续序号仍应被接受")
	assert.Equal(t, uint64(1), w.Max())
}

func TestReplayWindowAllowsJumpWithinWindow(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)
	require.True(t, w.Accept(0))

	// 恰好一个窗口宽度的前跳应被允许。
	assert.True(t, w.Accept(DefaultWindowSize), "窗口宽度内的前跳应被接受")
	assert.Equal(t, uint64(DefaultWindowSize), w.Max())
}

func TestReplayWindowShiftClearsOldBits(t *testing.T) {
	w := MustReplayWindow(64)

	require.True(t, w.Accept(0))
	require.False(t, w.Accept(0), "接受后应为已置位")

	// 推进整整一个窗口:0 号位对应的槽位被复用,
	// 但 0 本身已成为"过老"序号,不应再被接受。
	require.True(t, w.Accept(64))

	assert.False(t, w.Accept(0), "过老序号不得因位图复用而被重新接受")
	assert.False(t, w.Accept(64), "已接受的序号仍应被拒绝")
	assert.True(t, w.Accept(65), "新序号应被接受")
}

func TestReplayWindowInterleavedSequenceIsReplaySafe(t *testing.T) {
	// 模拟真实乱序场景:一组序号乱序到达,每个只应被接受一次。
	w := MustReplayWindow(DefaultWindowSize)

	incoming := []uint64{10, 8, 9, 11, 7, 13, 12, 6}
	accepted := make(map[uint64]int)

	for _, seq := range incoming {
		if w.Accept(seq) {
			accepted[seq]++
		}
	}
	for _, seq := range incoming {
		if w.Accept(seq) {
			accepted[seq]++
		}
	}

	for _, seq := range incoming {
		assert.Equal(t, 1, accepted[seq], "序号 %d 应恰好被接受一次", seq)
	}
}

func TestReplayWindowCheckDoesNotMutate(t *testing.T) {
	// Check 必须无副作用,否则"先验证后提交"的两阶段流程会失效。
	w := MustReplayWindow(DefaultWindowSize)

	assert.True(t, w.Check(10))
	assert.False(t, w.Initialized(), "Check 不应使窗口进入已初始化状态")
	assert.Equal(t, uint64(0), w.Max())

	require.True(t, w.Accept(10))
	assert.False(t, w.Check(10))
	assert.True(t, w.Check(11))
	assert.False(t, w.Check(10), "重复 Check 不应改变结果")
}

func TestReplayWindowReset(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)

	for seq := uint64(0); seq < 50; seq++ {
		require.True(t, w.Accept(seq))
	}
	require.False(t, w.Accept(0))

	w.Reset()

	assert.False(t, w.Initialized())
	assert.Equal(t, uint64(0), w.Max())
	assert.True(t, w.Accept(0), "重置后历史序号应可再次接受")
	assert.True(t, w.Accept(0) == false, "重置后重复仍应被拒绝")
}

func TestReplayWindowMaxIsMonotonic(t *testing.T) {
	w := MustReplayWindow(DefaultWindowSize)

	for _, seq := range []uint64{5, 100, 99, 200, 3, 150} {
		w.Accept(seq)
	}
	assert.Equal(t, uint64(200), w.Max(), "max 应单调不减")
}

func TestReplayWindowSizeAndInitialized(t *testing.T) {
	w := MustReplayWindow(128)
	assert.Equal(t, 128, w.Size())
	assert.False(t, w.Initialized())

	require.True(t, w.Accept(1))
	assert.True(t, w.Initialized())
}

func TestReplayWindowConcurrentAccess(t *testing.T) {
	// -race 下必须无报告:窗口是每包都要访问的热点结构。
	w := MustReplayWindow(DefaultWindowSize)

	const goroutines = 16
	const perGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				seq := base*perGoroutine + uint64(i)
				w.Check(seq)
				w.Accept(seq)
				w.Max()
			}
		}(uint64(g))
	}
	wg.Wait()

	// 本测试的目的是**并发安全**(由 -race 验证),而非"全部序号被接受":
	// ReplayWindow 对单次前跳超过一个窗口宽度的序号会拒绝(maxWindowShift),
	// 因此多个 goroutine 乱序推进 max 时,较低的序号可能因落后过远被丢弃,
	// Max() 不保证恰为全局最大序号。
	//
	// 断言 Max 落在合法范围内且非零,避免把并发交错误判为缺陷。
	assert.Greater(t, w.Max(), uint64(0))
	assert.LessOrEqual(t, w.Max(), uint64(goroutines*perGoroutine-1))
	assert.True(t, w.Initialized())
}

func TestReplayWindowConcurrentDuplicates(t *testing.T) {
	// 多个 goroutine 争抢同一批序号:每个序号应恰好被接受一次。
	w := MustReplayWindow(DefaultWindowSize)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted = make(map[uint64]int)
	)

	const workers = 8
	const seqs = 64

	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := uint64(0); seq < seqs; seq++ {
				if w.Accept(seq) {
					mu.Lock()
					accepted[seq]++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	require.Len(t, accepted, seqs, "所有序号都应被接受")
	for seq, n := range accepted {
		assert.Equal(t, 1, n, "序号 %d 被接受了 %d 次", seq, n)
	}
}
