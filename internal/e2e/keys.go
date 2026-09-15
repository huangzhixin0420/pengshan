// Package e2e 实现蓬山隧道的端到端加密层。
//
// 密钥学（docs/DEV-PLAN-PHASE1.md §2.3）：
//   - 双方各持 X25519 长期密钥对（daemon 首次启动生成；app 设备密钥对配对时生成）。
//   - 每次隧道建立双方再各生成一对 X25519 临时密钥，做 3-DH 混合派生：
//     s1 = ECDH(dev_long,  psn_long)   —— 配对时已有对方长期公钥
//     s2 = ECDH(e_dev,     psn_long)
//     s3 = ECDH(dev_long,  e_psn)
//     ikm = s1‖s2‖s3
//     salt = SHA256(hello_bytes ‖ respond_bytes)   —— transcript 绑定，防中间人拼会话
//     okm = HKDF-SHA256(ikm, salt, "pengshan-tunnel-v1")，64 字节 → 双向两个 32B key
//   - 数据帧用 XChaCha20-Poly1305 加密，nonce = 帧序号 n 的 8B 大端 ‖ 16B 零填充；
//     n 每条连接每方向单调递增，严格 +1 校验，重放/跳帧即断。
//
// 认证性说明：3-DH 中的 s1/s3 需要设备长期私钥、s2 需要 daemon 长期私钥；
// 长期公钥的交换发生在已认证通道（QR 含 daemon 公钥；配对 claim 含设备公钥），
// 因此握手完成 = 双方隐式互相认证；中间人没有任一方长期私钥即派生不出 key，
// 第一帧解密失败即断开。
package e2e

import (
	"crypto/ecdh"
	"crypto/sha256"
	"errors"
	"fmt"
)

const (
	// Info 是 HKDF 的 info 参数，绑定协议版本；协议演进时变更。
	Info = "pengshan-tunnel-v1"
	// KeyLen 是派生密钥长度（双向各 32B）。
	KeyLen = 64
	// MaxClockSkew 是握手时间戳允许的最大偏差（新鲜性检查）。
	MaxClockSkewSec = 300
)

var (
	ErrBadEnvelope = errors.New("e2e: malformed handshake envelope")
	ErrStaleTS     = errors.New("e2e: handshake timestamp out of window")
)

// Keys 是握手产出的方向密钥（send 加密本端发出帧，recv 解密对端来帧）。
type Keys struct {
	Send [32]byte
	Recv [32]byte
}

// IKM 拼三个 ECDH 共享秘密（顺序固定，双方一致）。
func ikm3(s1, s2, s3 []byte) []byte {
	out := make([]byte, 0, len(s1)+len(s2)+len(s3))
	out = append(out, s1...)
	out = append(out, s2...)
	out = append(out, s3...)
	return out
}

// derive 从三个共享秘密 + transcript 派生方向密钥。
// isInitiator=true 表示本端是 app（hello 发起方）：okm[0:32] 为 send，[32:64] 为 recv。
func derive(s1, s2, s3, transcriptSum []byte, isInitiator bool) (*Keys, error) {
	okm, err := hkdfSHA256(ikm3(s1, s2, s3), transcriptSum, []byte(Info), KeyLen)
	if err != nil {
		return nil, fmt.Errorf("hkdf: %w", err)
	}
	k := &Keys{}
	if isInitiator {
		copy(k.Send[:], okm[0:32])
		copy(k.Recv[:], okm[32:64])
	} else {
		copy(k.Send[:], okm[32:64])
		copy(k.Recv[:], okm[0:32])
	}
	return k, nil
}

// transcript 对两次握手消息做 SHA-256（salt）。
func transcript(hello, respond []byte) []byte {
	h := sha256.New()
	h.Write(hello)
	h.Write(respond)
	return h.Sum(nil)
}

// x25519 返回曲线实例（包内统一入口，避免各处重复 new）。
func x25519() ecdh.Curve { return ecdh.X25519() }

// mustShared 封装 ECDH 并统一报错文案。
func mustShared(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	s, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	return s, nil
}
