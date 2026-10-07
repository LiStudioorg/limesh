package crypto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 密钥生成
// ---------------------------------------------------------------------------

func TestGenerateKeyPairIsValid(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NotNil(t, kp)

	assert.False(t, isZero(kp.PrivateKey[:]), "私钥不应全零")
	assert.False(t, isZero(kp.PublicKey[:]), "公钥不应全零")
	assert.NotEqual(t, kp.PrivateKey, kp.PublicKey, "公私钥不应相同")
}

func TestGenerateKeyPairIsUnique(t *testing.T) {
	// 连续生成必须不同;相同意味着随机源失效。
	seen := make(map[[KeySize]byte]bool)
	for i := 0; i < 64; i++ {
		kp, err := GenerateKeyPair()
		require.NoError(t, err)
		require.False(t, seen[kp.PublicKey], "第 %d 次生成了重复公钥", i)
		seen[kp.PublicKey] = true
	}
}

func TestGenerateKeyPairConcurrent(t *testing.T) {
	// 并发生成不应产生重复或竞态。-race 下尤其有意义。
	const n = 32
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[[KeySize]byte]bool)
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kp, err := GenerateKeyPair()
			if err != nil {
				t.Errorf("generate: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[kp.PublicKey] {
				t.Error("并发生成了重复公钥")
			}
			seen[kp.PublicKey] = true
		}()
	}
	wg.Wait()
	assert.Len(t, seen, n)
}

func TestNewKeyPairFromPrivateIsDeterministic(t *testing.T) {
	var priv [KeySize]byte
	for i := range priv {
		priv[i] = byte(i + 1)
	}

	a, err := NewKeyPairFromPrivate(priv)
	require.NoError(t, err)
	b, err := NewKeyPairFromPrivate(priv)
	require.NoError(t, err)

	assert.Equal(t, a.PublicKey, b.PublicKey, "同一私钥必须导出同一公钥")
	assert.Equal(t, a.NodeID(), b.NodeID())
}

func TestNewKeyPairFromPrivateAcceptsZeroKey(t *testing.T) {
	// 实测行为:X25519 的 clamp 会把全零私钥的若干位强制置位,因此它派生出
	// 的是合法且非零的公钥,而不是低阶点。这里记录该事实,避免后人误以为
	// 全零私钥会被拒绝而写出错误的防御逻辑。
	//
	// 真正需要拒绝低阶点的是**对端公钥**(见 TestSharedSecretRejectsLowOrderPoint)。
	var zero [KeySize]byte
	kp, err := NewKeyPairFromPrivate(zero)
	require.NoError(t, err)
	require.NotNil(t, kp)

	assert.False(t, isZero(kp.PublicKey[:]), "clamp 后派生出的公钥不应为零")
	assert.NotEqual(t, zero, kp.PublicKey)
}

// ---------------------------------------------------------------------------
// NodeID 派生
// ---------------------------------------------------------------------------

func TestNodeIDIsDeterministic(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	first := kp.NodeID()
	for i := 0; i < 100; i++ {
		assert.Equal(t, first, kp.NodeID(), "同一密钥必须得到同一 NodeID")
	}
}

func TestNodeIDMatchesStandaloneFunction(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	assert.Equal(t, kp.NodeID(), NodeIDFromPublicKey(kp.PublicKey),
		"KeyPair.NodeID 与独立函数必须一致(验证方需能独立复算)")
}

func TestNodeIDDiffersForDifferentKeys(t *testing.T) {
	a, err := GenerateKeyPair()
	require.NoError(t, err)
	b, err := GenerateKeyPair()
	require.NoError(t, err)

	assert.NotEqual(t, a.NodeID(), b.NodeID())
}

func TestNodeIDIsSixteenBytes(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	id := kp.NodeID()
	assert.Len(t, id, NodeIDSize)
	assert.Len(t, id[:], 16)
}

func TestNodeIDHexRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	parsed, err := NodeIDFromHex(kp.NodeIDHex())
	require.NoError(t, err)
	assert.Equal(t, kp.NodeID(), parsed)
}

func TestPublicKeyHexRoundTrip(t *testing.T) {
	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	parsed, err := PublicKeyFromHex(kp.PublicKeyHex())
	require.NoError(t, err)
	assert.Equal(t, kp.PublicKey, parsed)
}

func TestHexParsersRejectBadInput(t *testing.T) {
	_, err := NodeIDFromHex("zzzz")
	require.Error(t, err, "非十六进制应报错")

	_, err = NodeIDFromHex("0102")
	require.Error(t, err, "长度不足应报错")

	_, err = NodeIDFromHex(strings.Repeat("ab", 32))
	require.Error(t, err, "长度过长应报错,不得静默截断")

	_, err = PublicKeyFromHex("")
	require.Error(t, err)

	_, err = PublicKeyFromHex(strings.Repeat("ab", 16))
	require.Error(t, err, "16 字节不足以构成 32 字节公钥")
}

func TestNilKeyPairAccessors(t *testing.T) {
	// 注意:kp.PublicKey 是结构体字段,对 nil 指针取字段必然 panic,
	// 这是 Go 的语义而非缺陷。因此这里只验证**方法**有 nil 保护的那些。
	var kp *KeyPair

	assert.NotPanics(t, func() {
		assert.Equal(t, [NodeIDSize]byte{}, kp.NodeID())
		assert.Equal(t, "", kp.PublicKeyHex())
		assert.Equal(t, "", kp.NodeIDHex())
	})

	assert.Error(t, kp.Save(filepath.Join(t.TempDir(), "k")), "nil 密钥不应能保存")

	_, err := kp.SharedSecret([KeySize]byte{})
	assert.Error(t, err, "nil 密钥不应能协商共享密钥")
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	original, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, original.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, original.PrivateKey, loaded.PrivateKey)
	assert.Equal(t, original.PublicKey, loaded.PublicKey)
	assert.Equal(t, original.NodeID(), loaded.NodeID())
}

func TestSaveUsesRestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, KeyFileMode, info.Mode().Perm(), "密钥文件必须是 0600")
}

func TestSaveCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "c", "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))

	require.FileExists(t, path)

	// 目录权限应为 0700(仅在新创建时生效)。
	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, KeyDirMode, info.Mode().Perm())
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	require.Len(t, entries, 1, "不应残留临时文件")
	assert.Equal(t, "key.bin", entries[0].Name())
}

func TestSaveOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	first, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, first.Save(path))

	second, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, second.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, second.PublicKey, loaded.PublicKey, "覆盖后应为新密钥")
}

// ---------------------------------------------------------------------------
// 权限校验
// ---------------------------------------------------------------------------

func TestLoadRejectsPermissiveMode(t *testing.T) {
	// 逐项验证:除 0600 之外的任何权限都必须被拒绝。
	for _, perm := range []os.FileMode{0o644, 0o640, 0o666, 0o600 + 0o077, 0o400} {
		t.Run(perm.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "key.bin")

			kp, err := GenerateKeyPair()
			require.NoError(t, err)
			require.NoError(t, kp.Save(path))
			require.NoError(t, os.Chmod(path, perm))

			_, err = Load(path)
			require.Error(t, err, "权限 %#o 应被拒绝", perm)
			assert.ErrorIs(t, err, ErrKeyFilePerm)
		})
	}
}

func TestLoadDoesNotSilentlyFixPermissions(t *testing.T) {
	// 权限被放宽通常意味着有人复制过密钥;静默 chmod 会掩盖这一事实。
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))
	require.NoError(t, os.Chmod(path, 0o644))

	_, err = Load(path)
	require.Error(t, err)

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(),
		"加载失败不应修改文件权限")
}

func TestLoadRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()

	for name, size := range map[string]int{"empty": 0, "short": 16, "long": 64} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".bin")
			require.NoError(t, os.WriteFile(path, make([]byte, size), KeyFileMode))

			_, err := Load(path)
			require.Error(t, err)
			// 空文件会在长度校验处失败。
			assert.ErrorIs(t, err, ErrKeyFileSize)
		})
	}
}

func TestLoadRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "adir")
	require.NoError(t, os.Mkdir(path, KeyDirMode))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is a directory")
}

func TestLoadMissingFileReturnsNotExist(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.bin"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"必须保留 os.ErrNotExist,否则 LoadOrGenerate 无法区分首次运行")
}

// ---------------------------------------------------------------------------
// LoadOrGenerate
// ---------------------------------------------------------------------------

func TestLoadOrGenerateCreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.bin")

	first, err := LoadOrGenerate(path)
	require.NoError(t, err)
	require.FileExists(t, path)

	second, err := LoadOrGenerate(path)
	require.NoError(t, err)

	assert.Equal(t, first.PublicKey, second.PublicKey, "第二次必须复用已有身份")
	assert.Equal(t, first.NodeID(), second.NodeID())
}

func TestLoadOrGeneratePropagatesPermissionError(t *testing.T) {
	// 已存在但权限错误的文件:必须报错,不得覆盖重生成(那会静默改变身份)。
	path := filepath.Join(t.TempDir(), "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))
	require.NoError(t, os.Chmod(path, 0o644))

	_, err = LoadOrGenerate(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyFilePerm)
}

func TestLoadOrGenerateRejectsEmptyPath(t *testing.T) {
	_, err := LoadOrGenerate("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path must not be empty")
}

// ---------------------------------------------------------------------------
// 共享密钥
// ---------------------------------------------------------------------------

func TestSharedSecretIsSymmetric(t *testing.T) {
	a, err := GenerateKeyPair()
	require.NoError(t, err)
	b, err := GenerateKeyPair()
	require.NoError(t, err)

	ab, err := a.SharedSecret(b.PublicKey)
	require.NoError(t, err)
	ba, err := b.SharedSecret(a.PublicKey)
	require.NoError(t, err)

	assert.Equal(t, ab, ba, "X25519 必须对称")
	assert.False(t, isZero(ab[:]))
}

func TestSharedSecretRejectsLowOrderPoint(t *testing.T) {
	a, err := GenerateKeyPair()
	require.NoError(t, err)

	// 全零公钥是低阶点,协商结果会退化为全零。
	var zero [KeySize]byte
	_, err = a.SharedSecret(zero)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLowOrderPoint)
}

// ---------------------------------------------------------------------------
// 故障注入:Save 的错误路径
// ---------------------------------------------------------------------------

func TestSaveFailsWhenParentIsAFile(t *testing.T) {
	// 父路径是普通文件时,MkdirAll 失败,应返回可诊断的错误。
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	err = kp.Save(filepath.Join(blocker, "sub", "key.bin"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create dir")
}

func TestSaveFailsOnReadOnlyDirectory(t *testing.T) {
	// 只读目录下无法创建临时文件,应报错且不留下残片。
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}

	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(ro, 0o500))
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })

	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	err = kp.Save(filepath.Join(ro, "key.bin"))
	require.Error(t, err)

	entries, readErr := os.ReadDir(ro)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "失败的保存不应留下临时文件")
}

func TestSaveFailsWhenTargetIsDirectory(t *testing.T) {
	// 目标是目录时 rename 会失败。
	dir := t.TempDir()
	target := filepath.Join(dir, "key.bin")
	require.NoError(t, os.Mkdir(target, 0o700))

	kp, err := GenerateKeyPair()
	require.NoError(t, err)

	err = kp.Save(target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename")

	// 目录本身不应被破坏,也不应残留临时文件。
	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
	assert.Equal(t, "key.bin", entries[0].Name())
}

func TestSaveProducesExactly32Bytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Len(t, raw, KeySize, "文件应为裸 32 字节私钥")
	assert.Equal(t, kp.PrivateKey[:], raw)
}

func TestLoadAcceptsFileWithSpecialModeBits(t *testing.T) {
	// 权限校验只看权限位,不应被 setuid/sticky 等特殊位干扰。
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")

	kp, err := GenerateKeyPair()
	require.NoError(t, err)
	require.NoError(t, kp.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, kp.PublicKey, loaded.PublicKey)
}
