// Package crypto 提供 Limesh 的长期身份密钥与派生工具。
//
// 职责边界:本包只负责身份的**生成、持久化与派生**,不实现握手与加密。
// Noise IK 握手见 core/handshake,会话加解密见 core/session。
//
// 设计说明见 AGENTS.md 第 8.2 节(身份与标识)与第 8.10 节(安全考虑)。
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// 密钥长度常量。Curve25519 的私钥与公钥均为 32 字节。
const (
	// KeySize 是 Curve25519 私钥/公钥的字节长度。
	KeySize = 32
	// NodeIDSize 是 NodeID 的字节长度(AGENTS.md 8.2)。
	NodeIDSize = 16
)

// 文件权限常量。
//
// 密钥文件按 docs/CONFIG.md 第 8 章要求强制 0600:密钥一旦被同机其他用户
// 读取,整个虚拟网络的身份隔离即失效。
const (
	// KeyFileMode 是密钥文件必须具有的权限。
	KeyFileMode os.FileMode = 0o600
	// KeyDirMode 是密钥目录的建议权限。
	KeyDirMode os.FileMode = 0o700
)

// 密钥相关错误。
var (
	// ErrKeyFilePerm 表示密钥文件权限过宽。
	ErrKeyFilePerm = errors.New("crypto: key file permission too permissive")
	// ErrKeyFileSize 表示密钥文件长度不合法。
	ErrKeyFileSize = errors.New("crypto: invalid key file size")
	// ErrLowOrderPoint 表示对端公钥是低阶点,会产生全零共享密钥。
	ErrLowOrderPoint = errors.New("crypto: low-order public key")
)

// KeyPair 是一对 Curve25519 长期身份密钥。
//
// 长期密钥用于 Noise IK 握手的静态密钥部分,决定了本节点的 NodeID。
// 密钥一旦变更,NodeID 随之变更,网络内其它节点将视其为新节点。
type KeyPair struct {
	// PrivateKey 是 Curve25519 私钥。
	PrivateKey [KeySize]byte
	// PublicKey 是 Curve25519 公钥,由私钥确定性导出。
	PublicKey [KeySize]byte
}

// GenerateKeyPair 生成一个新的 Curve25519 密钥对。
//
// 使用操作系统提供的密码学安全随机源;rand.Reader 失败时返回错误而非回退到
// 弱随机源——身份密钥的随机性不可妥协。
func GenerateKeyPair() (*KeyPair, error) {
	var priv [KeySize]byte
	if _, err := io.ReadFull(rand.Reader, priv[:]); err != nil {
		return nil, fmt.Errorf("crypto: generate key pair: read random: %w", err)
	}
	return keyPairFromPrivate(priv)
}

// keyPairFromPrivate 由私钥导出完整密钥对。
//
// Curve25519 的 clamp 由库函数负责;此处额外校验结果不是低阶点。
func keyPairFromPrivate(priv [KeySize]byte) (*KeyPair, error) {
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		if strings.Contains(err.Error(), "low order point") {
			return nil, fmt.Errorf("crypto: derive public key: %w: %w", ErrLowOrderPoint, err)
		}
		return nil, fmt.Errorf("crypto: derive public key: %w", err)
	}
	if isZero(pub) {
		return nil, fmt.Errorf("crypto: derive public key: %w", ErrLowOrderPoint)
	}

	kp := &KeyPair{PrivateKey: priv}
	copy(kp.PublicKey[:], pub)
	return kp, nil
}

// NewKeyPairFromPrivate 由 32 字节私钥构造密钥对。
//
// 用于从持久化内容恢复身份,以及测试中构造确定性密钥。
func NewKeyPairFromPrivate(priv [KeySize]byte) (*KeyPair, error) {
	return keyPairFromPrivate(priv)
}

// LoadOrGenerate 从 path 加载密钥;文件不存在时生成并保存。
//
// 权限策略(符合 docs/CONFIG.md 8 章):
//   - 已存在的文件:必须是 0600,否则拒绝加载(不自动修复,避免掩盖人为改动)
//   - 新生成的文件:以 0600 创建,父目录以 0700 创建
//
// 拒绝自动 chmod 修复是有意为之:权限被放宽通常意味着有人复制或改动过密钥
// 文件,静默修复会掩盖这一事实。
func LoadOrGenerate(path string) (*KeyPair, error) {
	if path == "" {
		return nil, fmt.Errorf("crypto: load or generate: path must not be empty")
	}

	kp, err := Load(path)
	switch {
	case err == nil:
		return kp, nil
	case errors.Is(err, os.ErrNotExist):
		// 继续走生成路径。
	default:
		return nil, err
	}

	kp, err = GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	if err := kp.Save(path); err != nil {
		return nil, err
	}
	return kp, nil
}

// Load 从 path 读取密钥对。
//
// 文件格式为**裸 32 字节私钥**(见 Save 的说明)。加载前强制校验权限。
func Load(path string) (*KeyPair, error) {
	info, err := os.Stat(path)
	if err != nil {
		// 保留 os.ErrNotExist 以便上层区分"不存在"与"不可读"。
		return nil, fmt.Errorf("crypto: load key %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("crypto: load key %q: is a directory", path)
	}

	// 只校验权限位,忽略文件类型位与特殊位。
	if perm := info.Mode().Perm(); perm != KeyFileMode {
		return nil, fmt.Errorf("crypto: load key %q: mode %#o: %w (want %#o)",
			path, perm, ErrKeyFilePerm, KeyFileMode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("crypto: load key %q: %w", path, err)
	}
	if len(raw) != KeySize {
		return nil, fmt.Errorf("crypto: load key %q: got %d bytes: %w (want %d)",
			path, len(raw), ErrKeyFileSize, KeySize)
	}

	var priv [KeySize]byte
	copy(priv[:], raw)
	return keyPairFromPrivate(priv)
}

// Save 把私钥以 0600 权限写入 path,必要时创建父目录。
//
// 文件格式选择**裸 32 字节私钥**,而非 PEM:
//
//   - 私钥是定长二进制,不存在需要 PEM 承载的结构化元数据;
//   - 免去 base64 编解码与 PEM 头解析,减少出错面;
//   - 定长文件让"长度校验"成为一道有效的完整性检查(见 ErrKeyFileSize);
//   - AGENTS.md 7.1 要求零 CGO 与最小依赖,不必要的编码格式是负担。
//
// 代价是文件不可读、无法通过文本工具辨认。对密钥文件而言这是可接受甚至
// 期望的性质:它不应被当作普通配置查看或复制粘贴。
//
// 写入采用"临时文件 + 原子改名",避免进程在写入中途退出留下截断的密钥文件
// ——截断的密钥文件会让节点在下次启动时身份改变。
func (k *KeyPair) Save(path string) error {
	if k == nil {
		return fmt.Errorf("crypto: save key: nil key pair")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, KeyDirMode); err != nil {
		return fmt.Errorf("crypto: save key %q: create dir: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".key-*.tmp")
	if err != nil {
		return fmt.Errorf("crypto: save key %q: create temp: %w", path, err)
	}
	tmpName := tmp.Name()

	// 任一失败路径都要清理临时文件,避免在密钥目录里留下残片。
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(KeyFileMode); err != nil {
		cleanup()
		return fmt.Errorf("crypto: save key %q: chmod: %w", path, err)
	}
	if _, err := tmp.Write(k.PrivateKey[:]); err != nil {
		cleanup()
		return fmt.Errorf("crypto: save key %q: write: %w", path, err)
	}
	// fsync 保证内容真正落盘后再改名,防止掉电后出现空文件。
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("crypto: save key %q: sync: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("crypto: save key %q: close: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("crypto: save key %q: rename: %w", path, err)
	}
	return nil
}

// NodeID 返回由公钥派生的节点身份:SHA256(pubkey)[:16]。
//
// NodeID 是**身份**而非地址:改 IP、换网卡、换中继都不改变它。
func (k *KeyPair) NodeID() [NodeIDSize]byte {
	var id [NodeIDSize]byte
	if k == nil {
		return id
	}
	sum := sha256.Sum256(k.PublicKey[:])
	copy(id[:], sum[:NodeIDSize])
	return id
}

// PublicKeyHex 返回公钥的十六进制表示,用于日志与配置比对。
func (k *KeyPair) PublicKeyHex() string {
	if k == nil {
		return ""
	}
	return hex.EncodeToString(k.PublicKey[:])
}

// NodeIDHex 返回 NodeID 的十六进制表示,用于日志与拓扑展示。
func (k *KeyPair) NodeIDHex() string {
	if k == nil {
		return ""
	}
	// 注意:不能直接对函数返回值取切片(不可寻址),需先赋给变量。
	id := k.NodeID()
	return hex.EncodeToString(id[:])
}

// SharedSecret 计算与对端公钥的 X25519 共享密钥。
//
// Phase 1a 的握手由 Noise 库内部完成密钥协商,本方法用于需要独立派生密钥的
// 场景(如 PSK 派生)与测试。对低阶点返回 ErrLowOrderPoint 而非全零结果
// ——全零共享密钥是已知的无效曲线攻击入口。
func (k *KeyPair) SharedSecret(peerPub [KeySize]byte) ([KeySize]byte, error) {
	var out [KeySize]byte
	if k == nil {
		return out, fmt.Errorf("crypto: shared secret: nil key pair")
	}

	shared, err := curve25519.X25519(k.PrivateKey[:], peerPub[:])
	if err != nil {
		// 标准库已拒绝低阶点,但返回的是它自己的错误类型。此处归一为
		// ErrLowOrderPoint,使调用方只需判断一个 sentinel;同时用 %w 链
		// 保留原始错误以便排查。
		if strings.Contains(err.Error(), "low order point") {
			return out, fmt.Errorf("crypto: shared secret: %w: %w", ErrLowOrderPoint, err)
		}
		return out, fmt.Errorf("crypto: shared secret: %w", err)
	}
	// 纵深防御:若某实现选择返回全零而非报错,此处仍需拦截。
	if isZero(shared) {
		return out, fmt.Errorf("crypto: shared secret: %w", ErrLowOrderPoint)
	}

	copy(out[:], shared)
	return out, nil
}

// NodeIDFromPublicKey 由公钥计算 NodeID。
//
// 独立于 KeyPair 存在,使验证方可以只用对端公钥复算 NodeID 并比对,
// 从而确认"声称的身份"与"握手得到的公钥"一致。
func NodeIDFromPublicKey(pub [KeySize]byte) [NodeIDSize]byte {
	var id [NodeIDSize]byte
	sum := sha256.Sum256(pub[:])
	copy(id[:], sum[:NodeIDSize])
	return id
}

// isZero 判断字节切片是否全零。
func isZero(b []byte) bool {
	var acc byte
	for _, v := range b {
		acc |= v
	}
	return acc == 0
}
