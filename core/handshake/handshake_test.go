package handshake

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LiStudioorg/limesh/core/crypto"
	"github.com/LiStudioorg/limesh/core/session"
)

// newPair 构造一对握手实例(发起方 A、响应方 B)。
func newPair(t *testing.T) (*Handshake, *Handshake, *crypto.KeyPair, *crypto.KeyPair) {
	t.Helper()

	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	initiator, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)

	responder, err := NewResponder(keyB)
	require.NoError(t, err)

	return initiator, responder, keyA, keyB
}

// runHandshake 执行一次完整的 1-RTT 握手,返回双方实例。
func runHandshake(t *testing.T) (*Handshake, *Handshake) {
	t.Helper()

	a, b, _, _ := newPair(t)

	// --- 消息 1:Init ---
	initMsg, err := a.WriteMessage()
	require.NoError(t, err)
	require.NotEmpty(t, initMsg)

	_, err = b.ReadMessage(initMsg)
	require.NoError(t, err, "响应方应接受 Init")

	// --- 消息 2:Resp ---
	respMsg, err := b.WriteMessage()
	require.NoError(t, err)
	require.NotEmpty(t, respMsg)

	_, err = a.ReadMessage(respMsg)
	require.NoError(t, err, "发起方应接受 Resp")

	// --- 消息 3:Complete ---
	completeMsg, err := a.WriteMessage()
	require.NoError(t, err)

	_, err = b.ReadMessage(completeMsg)
	require.NoError(t, err, "响应方应接受 Complete")

	return a, b
}

// ---------------------------------------------------------------------------
// 完整握手
// ---------------------------------------------------------------------------

func TestHandshakeCompletesRoundTrip(t *testing.T) {
	a, b := runHandshake(t)

	assert.True(t, a.Complete(), "发起方应认为握手已完成")
	assert.True(t, b.Complete(), "响应方应认为握手已完成")
	assert.Equal(t, StageEstablished, a.Stage())
	assert.Equal(t, StageEstablished, b.Stage())
}

func TestHandshakeEstablishesMutualIdentity(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)
	b, err := NewResponder(keyB)
	require.NoError(t, err)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)
	_, err = b.ReadMessage(initMsg)
	require.NoError(t, err)

	respMsg, err := b.WriteMessage()
	require.NoError(t, err)
	_, err = a.ReadMessage(respMsg)
	require.NoError(t, err)

	// 双方必须各自「看到」对方的正确身份。
	assert.Equal(t, keyB.NodeID(), a.RemoteNodeID(), "发起方应识别响应方")
	assert.Equal(t, keyA.NodeID(), b.RemoteNodeID(), "响应方应识别发起方")
	assert.Equal(t, keyA.PublicKey, b.RemotePublicKey(), "公钥应一致")
	assert.Equal(t, keyB.PublicKey, a.RemotePublicKey())
}

func TestHandshakeDerivesUsableSession(t *testing.T) {
	a, b := runHandshake(t)

	sessionA, err := a.Session()
	require.NoError(t, err)
	sessionB, err := b.Session()
	require.NoError(t, err)

	// 密钥必须交叉对应:A 的发送密钥 = B 的接收密钥。
	assert.Equal(t, sessionA.SendKey(), sessionB.RecvKey(), "A→B 密钥应交叉对应")
	assert.Equal(t, sessionA.RecvKey(), sessionB.SendKey(), "B→A 密钥应交叉对应")

	// 会话 ID 必须一致(由双方公钥确定性派生)。
	assert.Equal(t, sessionA.ID, sessionB.ID, "会话 ID 应一致")
}

func TestHandshakeSessionEncryptsBidirectional(t *testing.T) {
	// 这是握手是否真正成功的最终判据:派生出的密钥必须能双向加解密。
	a, b := runHandshake(t)

	sessionA, err := a.Session()
	require.NoError(t, err)
	sessionB, err := b.Session()
	require.NoError(t, err)

	// A → B
	seq1, ct1, err := sessionA.SealNext([]byte("hello from A"), nil)
	require.NoError(t, err)

	pt1, err := sessionB.Decrypt(seq1, ct1, nil)
	require.NoError(t, err, "B 应能解开 A 的密文")
	assert.Equal(t, []byte("hello from A"), pt1)

	// B → A
	seq2, ct2, err := sessionB.SealNext([]byte("hello from B"), nil)
	require.NoError(t, err)

	pt2, err := sessionA.Decrypt(seq2, ct2, nil)
	require.NoError(t, err, "A 应能解开 B 的密文")
	assert.Equal(t, []byte("hello from B"), pt2)
}

func TestHandshakeSessionIDsMatch(t *testing.T) {
	a, b := runHandshake(t)

	sa, err := a.Session()
	require.NoError(t, err)
	sb, err := b.Session()
	require.NoError(t, err)

	assert.Equal(t, sa.ID, sb.ID, "两端会话 ID 必须相同")

	// 会话 ID 由双方公钥导出,应可独立复算。
	expected := session.DeriveSessionID(a.RemotePublicKey(), a.local.PublicKey)
	assert.Equal(t, expected, sa.ID, "会话 ID 应是双方公钥的确定性函数")
}

// ---------------------------------------------------------------------------
// 密钥不匹配与攻击场景
// ---------------------------------------------------------------------------

func TestHandshakeRejectsWrongInitiatorKey(t *testing.T) {
	// 发起方用错误的响应方公钥构造握手:响应方必须无法解开。
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	wrongKey, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	// 发起方以为对端是 wrongKey(公钥不匹配)。
	a, err := NewInitiator(keyA, wrongKey.PublicKey)
	require.NoError(t, err)
	b, err := NewResponder(keyB)
	require.NoError(t, err)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	// MAC1 用错误的公钥派生,响应方校验必然失败。
	_, err = b.ReadMessage(initMsg)
	require.Error(t, err, "公钥不匹配的 Init 必须被拒绝")
}

func TestHandshakeRejectsTamperedInit(t *testing.T) {
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	// 翻转 Noise 载荷中的一位。
	tampered := append([]byte(nil), initMsg...)
	tampered[headerSize+timestampSize] ^= 0x01

	_, err = b.ReadMessage(tampered)
	require.Error(t, err, "被篡改的 Init 必须被拒绝")
}

func TestHandshakeRejectsTamperedMAC1(t *testing.T) {
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	tampered := append([]byte(nil), initMsg...)
	tampered[len(tampered)-MAC1OffsetFromEnd] ^= 0xFF

	_, err = b.ReadMessage(tampered)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMAC1)
}

func TestHandshakeRejectsBadMessageType(t *testing.T) {
	_, b, _, _ := newPair(t)

	_, err := b.ReadMessage([]byte{0xFF, 0, 0, 0})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadMessageType)
}

func TestHandshakeRejectsShortMessage(t *testing.T) {
	_, b, _, _ := newPair(t)

	_, err := b.ReadMessage([]byte{MsgTypeInit})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortMessage)

	_, err = b.ReadMessage(nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortMessage)
}

func TestHandshakeRejectsNonZeroReserved(t *testing.T) {
	// 保留字段非零必须被拒绝:它参与 MAC,是版本降级攻击的防线。
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	tampered := append([]byte(nil), initMsg...)
	tampered[1] = 0xAB

	_, err = b.ReadMessage(tampered)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReservedNotZero)
}

func TestHandshakeRejectsRoleMismatch(t *testing.T) {
	// 发起方不应处理 Init;响应方不应处理 Resp。
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)
	_, err = a.ReadMessage(initMsg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRoleMismatch)

	// 响应方尚未读到 Init,因此不应能写出 Resp。
	_, err = b.WriteMessage()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadStage)
}

// ---------------------------------------------------------------------------
// 重放防护
// ---------------------------------------------------------------------------

func TestHandshakeRejectsReplayedInit(t *testing.T) {
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	_, err = b.ReadMessage(initMsg)
	require.NoError(t, err, "首次应成功")

	// 重放同一条 Init:时间戳已被记录,必须拒绝。
	_, err = b.ReadMessage(initMsg)
	require.Error(t, err, "重放必须被拒绝")

	// 第二次可能因时间戳重复或阶段错误被拒,两者都可接受。
	assert.True(t,
		assert.ObjectsAreEqual(err, err))
}

func TestHandshakeRejectsOldTimestamp(t *testing.T) {
	// 时间戳超出窗口必须被拒绝,防止录制重放旧握手。
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	b, err := NewResponder(keyB)
	require.NoError(t, err)

	// 手工构造一条时间戳为 1 小时前的 Init。
	oldTS := uint64(time.Now().Add(-time.Hour).UnixNano())
	payload := make([]byte, 100)
	mac1Key := DeriveMAC1Key(keyB.PublicKey[:])
	msg := EncodeInit(1234, oldTS, payload, mac1Key, nil)

	_, err = b.ReadMessage(msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTimestampSkew)
}

func TestHandshakeRejectsFutureTimestamp(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	b, err := NewResponder(keyB)
	require.NoError(t, err)

	futureTS := uint64(time.Now().Add(time.Hour).UnixNano())
	mac1Key := DeriveMAC1Key(keyB.PublicKey[:])
	msg := EncodeInit(1, futureTS, make([]byte, 100), mac1Key, nil)

	_, err = b.ReadMessage(msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTimestampSkew)
}

func TestCheckTimestampBoundaries(t *testing.T) {
	now := uint64(time.Now().UnixNano())
	window := uint64(TimestampWindow.Nanoseconds())

	cases := map[string]struct {
		ts      uint64
		wantErr bool
	}{
		"now":              {now, false},
		"window behind":    {now - window, false},
		"just past window": {now - window - 1, true},
		"window ahead":     {now + window, false},
		"just past ahead":  {now + window + 1, true},
		"way behind":       {now - window*10, true},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := CheckTimestamp(tc.ts, now)
			if tc.wantErr {
				assert.ErrorIs(t, err, ErrTimestampSkew)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 超时
// ---------------------------------------------------------------------------

func TestHandshakeTimesOut(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewWithConfig(Config{
		Local:     keyA,
		RemotePub: &keyB.PublicKey,
		Initiator: true,
		Timeout:   20 * time.Millisecond,
	})
	require.NoError(t, err)

	assert.False(t, a.Expired())
	time.Sleep(40 * time.Millisecond)
	assert.True(t, a.Expired(), "超过 Timeout 应过期")

	_, err = a.WriteMessage()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTimeout)
}

func TestHandshakeReadAfterTimeout(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)
	b, err := NewResponder(keyB)
	require.NoError(t, err)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	// 人为把响应方的截止时间移到过去。
	b.mu.Lock()
	b.deadline = time.Now().Add(-time.Second)
	b.mu.Unlock()

	_, err = b.ReadMessage(initMsg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTimeout)
}

func TestDefaultTimeoutApplied(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)
	assert.Equal(t, HandshakeTimeout, a.timeout, "应使用 AGENTS.md 8.11 的超时常量")

	// 显式指定时应被采纳。
	b, err := NewWithConfig(Config{
		Local:     keyB,
		Initiator: false,
		Timeout:   2 * time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, b.timeout)
}

// ---------------------------------------------------------------------------
// 构造参数校验
// ---------------------------------------------------------------------------

func TestNewInitiatorRequiresRemoteKey(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	_, err = NewWithConfig(Config{Local: keyA, Initiator: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote public key")
}

func TestNewRequiresLocalKey(t *testing.T) {
	_, err := NewWithConfig(Config{Initiator: false})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local key pair")
}

func TestNewResponderNeedsNoRemoteKey(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	b, err := NewResponder(keyB)
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.False(t, b.Initiator())
	assert.Equal(t, StageInit, b.Stage())
}

// ---------------------------------------------------------------------------
// Prologue(网络隔离)
// ---------------------------------------------------------------------------

func TestPrologueMustMatch(t *testing.T) {
	// 绑定网络名称后,不同网络的节点即使密钥相同也无法互通。
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewWithConfig(Config{
		Local: keyA, RemotePub: &keyB.PublicKey, Initiator: true,
		Prologue: []byte("network-alpha"),
	})
	require.NoError(t, err)

	b, err := NewWithConfig(Config{
		Local: keyB, Initiator: false,
		Prologue: []byte("network-beta"),
	})
	require.NoError(t, err)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	_, err = b.ReadMessage(initMsg)
	require.Error(t, err, "prologue 不一致必须导致握手失败")
}

func TestPrologueMatchingSucceeds(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	prologue := []byte("network-alpha")

	a, err := NewWithConfig(Config{
		Local: keyA, RemotePub: &keyB.PublicKey, Initiator: true, Prologue: prologue,
	})
	require.NoError(t, err)
	b, err := NewWithConfig(Config{
		Local: keyB, Initiator: false, Prologue: prologue,
	})
	require.NoError(t, err)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)
	_, err = b.ReadMessage(initMsg)
	require.NoError(t, err)

	respMsg, err := b.WriteMessage()
	require.NoError(t, err)
	_, err = a.ReadMessage(respMsg)
	require.NoError(t, err)

	// 收到 Resp 后发起方已派生密钥(Noise 模式完成),可以取到会话;
	// 但状态机尚未进入 Established —— 那要等它把应用层 Complete 发出去。
	require.False(t, a.Complete(), "发出 Complete 前不应报告已完成")
	sessionA, err := a.Session()
	require.NoError(t, err, "收到 Resp 后密钥已可用")
	require.NotNil(t, sessionA)

	// 发起方发出应用层确认。
	completeMsg, err := a.WriteMessage()
	require.NoError(t, err)
	require.True(t, a.Complete(), "发出 Complete 后发起方应完成")

	_, err = b.ReadMessage(completeMsg)
	require.NoError(t, err)
	require.True(t, b.Complete(), "响应方收到 Complete 后应完成")

	sa, err := a.Session()
	require.NoError(t, err)
	sb, err := b.Session()
	require.NoError(t, err)

	// 密钥必须交叉对应。
	assert.Equal(t, sa.SendKey(), sb.RecvKey(), "A→B 密钥应交叉对应")
	assert.Equal(t, sa.RecvKey(), sb.SendKey(), "B→A 密钥应交叉对应")
}

// ---------------------------------------------------------------------------
// 阶段与生命周期
// ---------------------------------------------------------------------------

func TestWriteMessageRejectedWhenComplete(t *testing.T) {
	a, _ := runHandshake(t)

	_, err := a.WriteMessage()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrComplete)
}

func TestSessionBeforeCompletionFails(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)

	_, err = a.Session()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncomplete)

	_, err = a.WriteMessage()
	require.NoError(t, err)

	assert.False(t, a.Complete(), "发出 Init 后仍未完成")
	_, err = a.Session()
	require.Error(t, err, "尚未派生密钥时不应能取到会话")
}

func TestSenderIndexIsNonZeroAndUnique(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	seen := make(map[uint32]bool)
	for i := 0; i < 100; i++ {
		h, err := NewResponder(keyB)
		require.NoError(t, err)

		idx := h.SenderIndex()
		require.NotZero(t, idx, "索引 0 保留为未指定")
		require.False(t, seen[idx], "索引不应碰撞")
		seen[idx] = true
	}
}

func TestReceiverIndexPropagates(t *testing.T) {
	a, b, _, _ := newPair(t)

	initMsg, err := a.WriteMessage()
	require.NoError(t, err)

	parsed, err := ParseInit(initMsg)
	require.NoError(t, err)
	assert.Equal(t, a.SenderIndex(), parsed.SenderIndex)

	_, err = b.ReadMessage(initMsg)
	require.NoError(t, err)
	assert.Equal(t, a.SenderIndex(), b.ReceiverIndex(), "响应方应记录发起方索引")

	respMsg, err := b.WriteMessage()
	require.NoError(t, err)

	_, err = a.ReadMessage(respMsg)
	require.NoError(t, err)
	assert.Equal(t, b.SenderIndex(), a.ReceiverIndex(), "发起方应记录响应方索引")
}

func TestCloseReleasesSession(t *testing.T) {
	a, _ := runHandshake(t)

	require.NotNil(t, a)

	a.Close()
	assert.False(t, a.Complete(), "Close 后不应报告已完成")

	_, err := a.Session()
	require.Error(t, err)
}

func TestFingerprintIsStable(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewInitiator(keyA, keyB.PublicKey)
	require.NoError(t, err)

	fp1 := a.Fingerprint()
	fp2 := a.Fingerprint()
	assert.Equal(t, fp1, fp2, "同一公钥的指纹应稳定")
	assert.Len(t, fp1, 16, "应为 8 字节的十六进制")

	// 不同公钥应得到不同指纹。
	c, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	other, err := NewInitiator(keyA, c.PublicKey)
	require.NoError(t, err)
	assert.NotEqual(t, fp1, other.Fingerprint())
}

func TestLastErrorRecorded(t *testing.T) {
	keyA, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	a, err := NewWithConfig(Config{
		Local: keyA, RemotePub: &keyB.PublicKey, Initiator: true,
		Timeout: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)
	_, err = a.WriteMessage()
	require.Error(t, err)
	assert.ErrorIs(t, a.LastError(), ErrTimeout)
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestHandshakeConcurrentIndependentPairs(t *testing.T) {
	// -race 下必须无报告:多个握手并行进行时互不干扰。
	const pairs = 16

	var wg sync.WaitGroup
	for i := 0; i < pairs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			keyA, err := crypto.GenerateKeyPair()
			if err != nil {
				t.Errorf("keygen A: %v", err)
				return
			}
			keyB, err := crypto.GenerateKeyPair()
			if err != nil {
				t.Errorf("keygen B: %v", err)
				return
			}

			a, err := NewInitiator(keyA, keyB.PublicKey)
			if err != nil {
				t.Errorf("new initiator: %v", err)
				return
			}
			b, err := NewResponder(keyB)
			if err != nil {
				t.Errorf("new responder: %v", err)
				return
			}

			initMsg, err := a.WriteMessage()
			if err != nil {
				t.Errorf("write init: %v", err)
				return
			}
			if _, err := b.ReadMessage(initMsg); err != nil {
				t.Errorf("read init: %v", err)
				return
			}

			respMsg, err := b.WriteMessage()
			if err != nil {
				t.Errorf("write resp: %v", err)
				return
			}
			if _, err := a.ReadMessage(respMsg); err != nil {
				t.Errorf("read resp: %v", err)
				return
			}

			// 发起方还需要发出应用层 Complete,响应方才算完成。
			completeMsg, err := a.WriteMessage()
			if err != nil {
				t.Errorf("write complete: %v", err)
				return
			}
			if _, err := b.ReadMessage(completeMsg); err != nil {
				t.Errorf("read complete: %v", err)
				return
			}

			if !a.Complete() || !b.Complete() {
				t.Error("握手未完成")
				return
			}

			sa, err := a.Session()
			if err != nil {
				t.Errorf("session A: %v", err)
				return
			}
			sb, err := b.Session()
			if err != nil {
				t.Errorf("session B: %v", err)
				return
			}
			if sa.ID != sb.ID {
				t.Error("会话 ID 不一致")
			}
		}()
	}
	wg.Wait()
}

func TestHandshakeConcurrentAccessors(t *testing.T) {
	a, _ := runHandshake(t)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				a.Complete()
				a.Stage()
				a.SenderIndex()
				a.ReceiverIndex()
				a.RemoteNodeID()
				a.RemotePublicKey()
				a.Fingerprint()
				a.Expired()
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 线格式
// ---------------------------------------------------------------------------

func TestInitMessageLayout(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	mac1Key := DeriveMAC1Key(keyB.PublicKey[:])
	payload := []byte("noise-payload")

	msg := EncodeInit(0xDEADBEEF, 12345, payload, mac1Key, nil)

	assert.Equal(t, MsgTypeInit, msg[0], "首字节应为类型")
	assert.Equal(t, []byte{0, 0, 0}, msg[1:4], "保留字段应为零")

	// 总长度 = 头(8) + 时间戳(12) + 载荷 + 两个 MAC(32)
	assert.Len(t, msg, headerSize+timestampSize+len(payload)+2*macSize)

	// MAC1 覆盖 [头 | 时间戳 | 载荷],不含自身与 MAC2。
	require.NoError(t, VerifyMAC1(mac1Key, msg))
}

func TestRespMessageLayout(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)

	mac1Key := DeriveMAC1Key(keyB.PublicKey[:])
	msg := EncodeResp(1, 2, []byte("payload"), mac1Key, nil)

	assert.Equal(t, MsgTypeResp, msg[0])
	assert.Len(t, msg, headerSize+4+len("payload")+2*macSize)
	require.NoError(t, VerifyMAC1(mac1Key, msg))
}

func TestCompleteMessageParses(t *testing.T) {
	mac1Key := DeriveMAC1Key([]byte("some-key"))
	msg := EncodeComplete(7, 8, mac1Key)

	sender, receiver, _, err := ParseComplete(msg)
	require.NoError(t, err)
	assert.Equal(t, uint32(7), sender)
	assert.Equal(t, uint32(8), receiver)
	require.NoError(t, VerifyMAC1(mac1Key, msg))
}

func TestParseInitRoundTrip(t *testing.T) {
	keyB, err := crypto.GenerateKeyPair()
	require.NoError(t, err)
	mac1Key := DeriveMAC1Key(keyB.PublicKey[:])

	ts := uint64(time.Now().UnixNano())
	initMsg := EncodeInit(42, ts, []byte("payload"), mac1Key, nil)

	parsed, err := ParseInit(initMsg)
	require.NoError(t, err)
	assert.Equal(t, uint32(42), parsed.SenderIndex)
	assert.Equal(t, ts, parsed.Timestamp)
	assert.Equal(t, []byte("payload"), parsed.Payload)
}

func TestParseRespRoundTrip(t *testing.T) {
	mac1Key := DeriveMAC1Key([]byte("k"))
	msg := EncodeResp(11, 22, []byte("resp"), mac1Key, nil)

	parsed, err := ParseResp(msg)
	require.NoError(t, err)
	assert.Equal(t, uint32(11), parsed.SenderIndex)
	assert.Equal(t, uint32(22), parsed.ReceiverIndex)
	assert.Equal(t, []byte("resp"), parsed.Payload)
}

func TestVerifyMAC1RejectsShortMessage(t *testing.T) {
	err := VerifyMAC1([]byte("key"), []byte{1, 2, 3})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortMessage)
}

func TestVerifyMAC2NoCookieSkipsCheck(t *testing.T) {
	// cookie 为空表示本端未签发 cookie,校验应跳过而非失败。
	assert.NoError(t, VerifyMAC2(nil, []byte("anything")))

	// 提供了 cookie 但消息过短,应报错。
	err := VerifyMAC2([]byte("cookie"), []byte{1})
	require.Error(t, err)
}

func TestVerifyMAC2WithCookie(t *testing.T) {
	cookie := []byte("cookie-key")
	msg := EncodeInit(1, 2, []byte("payload"), nil, cookie)

	// MAC2 覆盖 [消息 | MAC1]。
	require.NoError(t, VerifyMAC2(cookie, msg))

	// 错误 cookie 必须失败。
	err := VerifyMAC2([]byte("wrong-cookie"), msg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMAC2)
}

func TestDeriveKeysAreStable(t *testing.T) {
	pub := []byte("public-key-bytes")

	k1 := DeriveMAC1Key(pub)
	k2 := DeriveMAC1Key(pub)
	assert.True(t, bytes.Equal(k1, k2), "同一公钥应派生出同一 MAC1 密钥")

	other := DeriveMAC1Key([]byte("other-key"))
	assert.False(t, bytes.Equal(k1, other))

	// cookie 密钥绑定来源地址,从而无法被重放到其它地址。
	c1 := DeriveCookieKey(pub, "1.2.3.4:11010")
	c2 := DeriveCookieKey(pub, "5.6.7.8:11010")
	assert.False(t, bytes.Equal(c1, c2), "不同来源地址应得到不同 cookie 密钥")
}

func TestMessageTypeHelper(t *testing.T) {
	typ, err := MessageType([]byte{MsgTypeInit})
	require.NoError(t, err)
	assert.Equal(t, MsgTypeInit, typ)

	_, err = MessageType(nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrShortMessage)
}
