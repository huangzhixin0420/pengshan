// Package identity 管理 daemon 的长期身份密钥对（X25519）。
// 首次启动生成 32B 随机私钥，0600 存于 ~/.pengshan/identity.key；
// 公钥经配对 QR 分发给 app（app 用它验握手 transcript）。
package identity

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/huangzhixin0420/pengshan/internal/config"
)

const FileName = "identity.key"

// Store 是长期私钥的加载/生成入口。
type Store struct {
	path string
}

// NewStore 默认存到 config.Dir() 下。
func NewStore() *Store {
	return &Store{path: filepath.Join(config.Dir(), FileName)}
}

// NewStoreAt 指定路径（测试注入临时目录）。
func NewStoreAt(path string) *Store { return &Store{path: path} }

// Path 返回私钥文件路径。
func (s *Store) Path() string { return s.path }

// LoadOrCreate 读私钥；不存在则生成并落盘（0600）。
func (s *Store) LoadOrCreate() (*ecdh.PrivateKey, error) {
	data, err := os.ReadFile(s.path)
	if err == nil {
		priv, err := ecdh.X25519().NewPrivateKey(data)
		if err != nil {
			return nil, fmt.Errorf("identity: corrupt %s (not X25519): %w", s.path, err)
		}
		return priv, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("identity: read %s: %w", s.path, err)
	}
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.path, priv.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("identity: write %s: %w", s.path, err)
	}
	return priv, nil
}

// Fingerprint 是公钥的 16 进制缩写（status/QR 展示用，全量公钥在 QR 载荷里）。
func Fingerprint(pub *ecdh.PublicKey) string {
	b := pub.Bytes()
	return fmt.Sprintf("%x", b[:8])
}
