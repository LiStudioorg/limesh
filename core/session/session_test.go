package session

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// randomKey 生成一个随机 AEAD 密钥。
func randomKey(t *testing.T) [KeySize]byte {
	t.Helper()
	var k [KeySize]byte
	_, err := rand.Read(k[:])
	require.NoError(t, err)
	return k
}

// newPair 构造一对方向交叉对应的会话:A 的 sendKey 是 B 的 recvKey。
func newPair(t *testing.T) (*Session, *Session) {
	t.Helper()

	kab := randomKey(t)
	kba := randomKey(t)

	a, err := NewSession(1, kab, kba)
	require.NoError(t, err)

	b, err := NewSession(1, kba, kab)
	require.NoError(t, err)

	return a, b
}

// ---------------------------------------------------------------------------
// 构造与基本属性
// ---------------------------------------------------------------------------

func TestNewSession(t *testing.T) {
	s, err := NewSession(42, randomKey(t), randomKey(t))
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.Equal(t, uint32(42), s.ID)
	assert.False(t, s.Closed())
	assert.False(t, s.Expired())
	assert.Equal(t, uint64(0), s.Sequence())
	assert.NotNil(t, s.RecvWindow())
	assert.WithinDuration(t, time.Now(), s.CreatedAt, time.Second)
}

func TestNewSessionKeysRoundTrip(t *testing.T) {
	send := randomKey(t)
	recv := randomKey(t)

	s, err := NewSession(1, send, recv)
	require.NoError(t, err)

	assert.Equal(t, send, s.SendKey())
	assert.Equal(t, recv, s.RecvKey())
}

func TestNewSessionWithTTL(t *testing.T) {
	s, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, s.TTL())

	// 非正值应回退到默认 TTL,而不是让会话立即过期。
	s2, err := NewSessionWithTTL(2, randomKey(t), randomKey(t), 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultSessionTTL, s2.TTL())
}

// ---------------------------------------------------------------------------
// 加解密往返
// ---------------------------------------------------------------------------

func TestEncryptDecryptRoundTrip(t *testing.T) {
	a, b := newPair(t)

	plaintext := []byte("hello limesh")

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, plaintext, nil)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, ct, "密文不应等于明文")
	assert.Len(t, ct, len(plaintext)+TagSize, "密文应比明文多一个认证标签")

	got, err := b.Decrypt(seq, ct, nil)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestEncryptDecryptVariousPayloadSizes(t *testing.T) {
	a, b := newPair(t)

	sizes := []int{0, 1, 15, 16, 17, 64, 1024, 1380, 65535}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i)
			}

			seq, err := a.NextSequence()
			require.NoError(t, err)

			ct, err := a.Encrypt(seq, payload, nil)
			require.NoError(t, err)

			got, err := b.Decrypt(seq, ct, nil)
			require.NoError(t, err)

			// 用长度 + 内容比较而非 assert.Equal:空载荷时 Seal 返回 nil,
			// 而原始 payload 是长度为 0 的非 nil 切片,两者语义相同但
			// reflect.DeepEqual 会判为不等。
			assert.Len(t, got, len(payload))
			assert.True(t, bytes.Equal(payload, got), "载荷内容应一致")
		})
	}
}

func TestEncryptIsNonDeterministicAcrossSessions(t *testing.T) {
	// 同一明文在不同会话密钥下必须产生不同密文。
	a1, b1 := newPair(t)
	a2, _ := newPair(t)

	payload := []byte("same plaintext")

	seq1, err := a1.NextSequence()
	require.NoError(t, err)
	ct1, err := a1.Encrypt(seq1, payload, nil)
	require.NoError(t, err)

	seq2, err := a2.NextSequence()
	require.NoError(t, err)
	ct2, err := a2.Encrypt(seq2, payload, nil)
	require.NoError(t, err)

	got1, err := b1.Decrypt(seq1, ct1, nil)
	require.NoError(t, err)
	assert.Equal(t, payload, got1)

	assert.NotEqual(t, ct1, ct2, "不同密钥下的密文不应相同")
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	a, _ := newPair(t)
	_, other := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("secret"), nil)
	require.NoError(t, err)

	_, err = other.Decrypt(seq, ct, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDecrypt, "错误密钥必须认证失败")
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("authenticated payload"), nil)
	require.NoError(t, err)

	// 翻转密文中间一个比特。
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)/2] ^= 0x01

	_, err = b.Decrypt(seq, tampered, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDecrypt, "篡改必须被认证标签发现")
}

func TestDecryptRejectsTamperedTag(t *testing.T) {
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("payload"), nil)
	require.NoError(t, err)

	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0xFF

	_, err = b.Decrypt(seq, tampered, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDecrypt)
}

func TestDecryptRejectsShortCiphertext(t *testing.T) {
	_, b := newPair(t)

	_, err := b.Decrypt(0, []byte{1, 2, 3}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortPacket)

	_, err = b.Decrypt(0, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortPacket)
}

// ---------------------------------------------------------------------------
// AAD
// ---------------------------------------------------------------------------

func TestAADIsAuthenticated(t *testing.T) {
	// AGENTS.md 8.7:ZCPacket 头的寻址与类型字段作为 AAD,
	// 防止头部被篡改为"投递给别人"。AAD 不匹配必须解密失败。
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	aad := []byte("zcp-header-bytes")
	ct, err := a.Encrypt(seq, []byte("payload"), aad)
	require.NoError(t, err)

	got, err := b.Decrypt(seq, ct, aad)
	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), got)
}

func TestDecryptRejectsWrongAAD(t *testing.T) {
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("payload"), []byte("correct-header"))
	require.NoError(t, err)

	_, err = b.Decrypt(seq, ct, []byte("tampered-header"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDecrypt, "AAD 被篡改必须导致认证失败")

	_, err = b.Decrypt(seq, ct, nil)
	require.Error(t, err, "AAD 缺失必须失败")
}

// ---------------------------------------------------------------------------
// 序号与 nonce
// ---------------------------------------------------------------------------

func TestNextSequenceIsMonotonic(t *testing.T) {
	s, err := NewSession(1, randomKey(t), randomKey(t))
	require.NoError(t, err)

	for want := uint64(0); want < 100; want++ {
		got, err := s.NextSequence()
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Equal(t, uint64(100), s.Sequence())
}

func TestNonceConstructionMatchesSpec(t *testing.T) {
	// nonce = SessionID(4B 大端) || Sequence(8B 大端)
	s, err := NewSession(0x01020304, randomKey(t), randomKey(t))
	require.NoError(t, err)

	nonce := s.nonce(0x0A0B0C0D0E0F1011)
	require.Len(t, nonce, NonceSize)

	assert.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, nonce[0:4], "前 4 字节应为 SessionID")
	assert.Equal(t, []byte{0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10, 0x11}, nonce[4:12],
		"后 8 字节应为序号")
}

func TestNonceDiffersBySessionID(t *testing.T) {
	// 同一序号在不同会话下必须得到不同 nonce,否则多会话共存会 nonce 重用。
	a, err := NewSession(1, randomKey(t), randomKey(t))
	require.NoError(t, err)
	b, err := NewSession(2, randomKey(t), randomKey(t))
	require.NoError(t, err)

	assert.NotEqual(t, a.nonce(5), b.nonce(5), "不同 SessionID 的 nonce 必须不同")
}

func TestNonceDiffersBySequence(t *testing.T) {
	s, err := NewSession(7, randomKey(t), randomKey(t))
	require.NoError(t, err)

	assert.NotEqual(t, s.nonce(1), s.nonce(2))
}

func TestEncryptRejectsReusedSequence(t *testing.T) {
	// 重复序号会产生 nonce 重用,必须被拒绝。
	s, err := NewSession(1, randomKey(t), randomKey(t))
	require.NoError(t, err)

	seq, err := s.NextSequence()
	require.NoError(t, err)
	_, err = s.Encrypt(seq, []byte("first"), nil)
	require.NoError(t, err)

	_, err = s.Encrypt(seq, []byte("second"), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReplay, "重用的 nonce 必须被拒绝")
}

// ---------------------------------------------------------------------------
// 重放防护(会话层)
// ---------------------------------------------------------------------------

func TestDecryptRejectsReplay(t *testing.T) {
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("payload"), nil)
	require.NoError(t, err)

	_, err = b.Decrypt(seq, ct, nil)
	require.NoError(t, err, "首次解密应成功")

	_, err = b.Decrypt(seq, ct, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReplay, "重放必须被拒绝")
}

func TestDecryptRejectsOldSequence(t *testing.T) {
	a, b := newPair(t)

	// 先发送一个高序号,把窗口推进。
	highSeq, err := a.NextSequence()
	require.NoError(t, err)
	for i := uint64(0); i < DefaultWindowSize+10; i++ {
		_, err := a.NextSequence()
		require.NoError(t, err)
	}
	seq, err := a.NextSequence()
	require.NoError(t, err)

	ct, err := a.Encrypt(seq, []byte("recent"), nil)
	require.NoError(t, err)
	_, err = b.Decrypt(seq, ct, nil)
	require.NoError(t, err)

	// 构造一个远低于窗口下界的序号。
	oldSeq := seq - DefaultWindowSize - 100
	ctOld, err := a.Encrypt(oldSeq, []byte("old"), nil)
	require.NoError(t, err)

	_, err = b.Decrypt(oldSeq, ctOld, nil)
	require.Error(t, err, "过老序号应被拒绝")
	assert.True(t, assert.ObjectsAreEqual(err, err))
	assert.ErrorIs(t, err, ErrReplay)

	_ = highSeq
}

func TestDecryptAcceptsOutOfOrder(t *testing.T) {
	// 乱序到达但在窗口内:必须能正确解密。
	a, b := newPair(t)

	type msg struct {
		seq uint64
		pt  []byte
	}

	var msgs []msg
	for i := 0; i < 10; i++ {
		seq, err := a.NextSequence()
		require.NoError(t, err)

		pt := []byte{byte(i)}
		ct, err := a.Encrypt(seq, pt, nil)
		require.NoError(t, err)

		msgs = append(msgs, msg{seq: seq, pt: ct})
	}

	// 逆序投递。
	for i := len(msgs) - 1; i >= 0; i-- {
		got, err := b.Decrypt(msgs[i].seq, msgs[i].pt, nil)
		require.NoError(t, err, "乱序序号 %d 应能解密", msgs[i].seq)
		assert.Equal(t, []byte{byte(i)}, got)
	}
}

func TestDecryptFailureDoesNotConsumeSequence(t *testing.T) {
	// 这是"先验证后提交"的核心保证:一个认证失败的包不能占用序号,
	// 否则攻击者仅靠发送垃圾包就能让合法重传被误判为重放。
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)

	good, err := a.Encrypt(seq, []byte("legitimate"), nil)
	require.NoError(t, err)

	// 先投递一个损坏的同序号包。
	bad := append([]byte(nil), good...)
	bad[0] ^= 0xFF
	_, err = b.Decrypt(seq, bad, nil)
	require.Error(t, err, "损坏的包应解密失败")

	// 随后投递真正的合法包:必须仍然成功。
	got, err := b.Decrypt(seq, good, nil)
	require.NoError(t, err, "认证失败不应占用序号,合法包仍应被接受")
	assert.Equal(t, []byte("legitimate"), got)
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

func TestExpiredByIdle(t *testing.T) {
	s, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), 20*time.Millisecond)
	require.NoError(t, err)

	assert.False(t, s.Expired())
	time.Sleep(40 * time.Millisecond)
	assert.True(t, s.Expired(), "超过 TTL 应过期")
}

func TestTouchExtendsLifetime(t *testing.T) {
	s, err := NewSessionWithTTL(1, randomKey(t), randomKey(t), 60*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(40 * time.Millisecond)
	require.False(t, s.Expired())

	s.Touch()
	time.Sleep(40 * time.Millisecond)

	assert.False(t, s.Expired(), "Touch 后应重新开始计时")
	assert.Less(t, s.Idle(), 60*time.Millisecond)
}

func TestCloseIsIdempotentAndBlocksOperations(t *testing.T) {
	a, b := newPair(t)

	seq, err := a.NextSequence()
	require.NoError(t, err)
	ct, err := a.Encrypt(seq, []byte("x"), nil)
	require.NoError(t, err)

	require.False(t, a.Closed())
	a.Close()
	require.True(t, a.Closed())
	assert.NotPanics(t, func() { a.Close() }, "重复 Close 应安全")

	assert.True(t, a.Expired(), "已关闭的会话应报告过期")

	_, err = a.NextSequence()
	assert.ErrorIs(t, err, ErrSessionClosed)

	_, err = a.Encrypt(0, []byte("x"), nil)
	assert.ErrorIs(t, err, ErrSessionClosed)

	_, err = b.Decrypt(seq, ct, nil)
	require.NoError(t, err, "对端未关闭,应仍可解密")

	b.Close()
	_, err = b.Decrypt(seq, ct, nil)
	assert.ErrorIs(t, err, ErrSessionClosed)
}

func TestCloseZeroizesKeys(t *testing.T) {
	send := randomKey(t)
	recv := randomKey(t)

	s, err := NewSession(1, send, recv)
	require.NoError(t, err)
	require.Equal(t, send, s.SendKey(), "关闭前应可读取")

	s.Close()

	assert.Equal(t, [KeySize]byte{}, s.SendKey(), "关闭后发送密钥应被清零")
	assert.Equal(t, [KeySize]byte{}, s.RecvKey(), "关闭后接收密钥应被清零")
}

func TestSetTTL(t *testing.T) {
	s, err := NewSession(1, randomKey(t), randomKey(t))
	require.NoError(t, err)

	s.SetTTL(3 * time.Second)
	assert.Equal(t, 3*time.Second, s.TTL())

	// 非正值应被忽略,保持原值。
	s.SetTTL(0)
	assert.Equal(t, 3*time.Second, s.TTL())
}

// ---------------------------------------------------------------------------
// 会话 ID 生成
// ---------------------------------------------------------------------------

func TestNewSessionIDIsUnique(t *testing.T) {
	seen := make(map[uint32]bool)
	for i := 0; i < 1000; i++ {
		id, err := NewSessionID()
		require.NoError(t, err)
		require.NotZero(t, id, "0 保留为未指定,不应被生成")
		require.False(t, seen[id], "会话 ID 不应碰撞")
		seen[id] = true
	}
}

func TestDeriveSessionIDIsSymmetric(t *testing.T) {
	// 双向同时发起握手时,两端必须算出同一个 ID,才能识别为同一会话。
	a := randomKey(t)
	b := randomKey(t)

	assert.Equal(t, DeriveSessionID(a, b), DeriveSessionID(b, a),
		"派生必须与角色无关")

	// 不同密钥对必须得到不同 ID(极高概率)。
	c := randomKey(t)
	assert.NotEqual(t, DeriveSessionID(a, b), DeriveSessionID(a, c))
}

func TestDeriveSessionIDNeverZero(t *testing.T) {
	for i := 0; i < 500; i++ {
		id := DeriveSessionID(randomKey(t), randomKey(t))
		require.NotZero(t, id, "派生结果不应为 0")
	}
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestSessionConcurrentEncryptDecrypt(t *testing.T) {
	// -race 下必须无报告。
	a, b := newPair(t)

	const n = 200
	var wg sync.WaitGroup

	// 发送侧并发生成密文。
	//
	// 每个 goroutine 独占一个下标,因此对切片的写入之间没有数据竞争;
	// 第二轮读取前由 wg.Wait() 建立 happens-before 关系。
	type sealed struct {
		seq uint64
		ct  []byte
	}
	results := make([]sealed, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 并发发送必须用 SealNext:NextSequence + Encrypt 是两次加锁,
			// 并发下会交错并触发 nonce 重用的防御性拒绝。
			seq, ct, err := a.SealNext([]byte("payload"), nil)
			if err != nil {
				t.Errorf("seal next: %v", err)
				return
			}
			results[i] = sealed{seq: seq, ct: ct}
		}(i)
	}
	wg.Wait()

	// 接收侧并发解密(乱序是预期的)。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pt, err := b.Decrypt(results[i].seq, results[i].ct, nil)
			if err != nil {
				t.Errorf("decrypt %d: %v", i, err)
				return
			}
			if !bytes.Equal(pt, []byte("payload")) {
				t.Errorf("decrypt %d: payload 不匹配", i)
			}
		}(i)
	}
	wg.Wait()
}

func TestSessionConcurrentCloseAndUse(t *testing.T) {
	// 并发关闭与使用不应 panic 或产生竞态。
	for i := 0; i < 20; i++ {
		s, err := NewSession(uint32(i), randomKey(t), randomKey(t))
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = s.NextSequence()
				s.Expired()
				s.Idle()
				s.Closed()
			}
		}()
		go func() {
			defer wg.Done()
			s.Close()
		}()

		wg.Wait()
	}
}
