package e2e

import (
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// 帧类型（tunnel data socket 的 JSON 帧）。
const (
	FrameData   = "data"
	FramePing   = "ping"
	FramePong   = "pong"
	FrameClose  = "close"  // body 加密：含 reason
	FrameErr    = "err"    // body 加密：含错误文案
	frameVer    = 1
	nonceSize   = chacha20poly1305.NonceSizeX // 24
	maxJump     = 1024                        // 序号跳变容忍（超出即断，防位翻转/重放）
	firstSeq    = 1                           // 序号从 1 起
)

var (
	ErrReplay   = errors.New("e2e: frame sequence replay or jump")
	ErrFrame    = errors.New("e2e: malformed frame")
	ErrDecrypt  = errors.New("e2e: frame decrypt failed")
	ErrOverhead = errors.New("e2e: plaintext too large")
)

// maxPlaintext 是单帧明文上限（64KB，与 tunnel 分块对齐）。
const maxPlaintext = 64 * 1024

// ReadLimit 是 WS 单条消息读上限：64KB 明文 base64 后 ≈87KB JSON，
// 1MB 留足余量（含 close/err 的 reason 与控制帧）。
const ReadLimit = 1 << 20

// Frame 是隧道线上格式。B 对 data/close/err 为密文 base64；ping/pong 恒为空。
type Frame struct {
	V uint32 `json:"v"`
	N uint64 `json:"n"`
	T string `json:"t"`
	B string `json:"b,omitempty"`
}

// Cipher 负责单方向帧的封/解与序号治理。两端各两个（send/recv）。
// 非并发安全：send 与 recv 各属一个 goroutine（WS 一写一读模型）。
type Cipher struct {
	aead   cipher.AEAD
	next   uint64 // 发送：下一帧序号；接收：期望序号
	isSend bool
}

// NewCipher 以 32B 方向密钥构造。isSend=true 为发送方向。
func NewCipher(key [32]byte, isSend bool) *Cipher {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		panic(err) // 32B key 不可能失败
	}
	return &Cipher{aead: aead, next: firstSeq, isSend: isSend}
}

// nonceFor 把序号编码进 24B nonce：8B 大端 ‖ 16B 零。
// XChaCha20 的宽 nonce 允许这种结构化填充；key 每连接唯一，单调序号保证唯一性。
func nonceFor(n uint64) []byte {
	nonce := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(nonce[:8], n)
	return nonce
}

// Seal 封一帧并推进序号。plaintext 上限 maxPlaintext（控制单帧内存与 WS 消息大小）。
func (c *Cipher) Seal(frameType string, plaintext []byte) ([]byte, error) {
	if len(plaintext) > maxPlaintext {
		return nil, fmt.Errorf("%w: %d > %d", ErrOverhead, len(plaintext), maxPlaintext)
	}
	f := Frame{V: frameVer, N: c.next, T: frameType}
	switch frameType {
	case FramePing, FramePong:
		// 控制帧不加密、无 body
	default:
		ct := c.aead.Seal(nil, nonceFor(c.next), plaintext, nil)
		f.B = b64u(ct)
	}
	out, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	c.next++
	return out, nil
}

// Open 解一帧并推进期望序号。ping/pong 返回空 plaintext。
func (c *Cipher) Open(data []byte) (frameType string, plaintext []byte, err error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrFrame, err)
	}
	if f.V != frameVer {
		return "", nil, fmt.Errorf("%w: version", ErrFrame)
	}
	// 重放/跳变检测：只允许严格 +1；跳变大（>maxJump）给出更明确的报错。
	if f.N != c.next {
		if f.N > c.next && f.N-c.next <= maxJump {
			return "", nil, fmt.Errorf("%w: gap %d (want %d)", ErrReplay, f.N, c.next)
		}
		return "", nil, fmt.Errorf("%w: got %d want %d", ErrReplay, f.N, c.next)
	}
	c.next++
	switch f.T {
	case FramePing, FramePong:
		return f.T, nil, nil
	case FrameData, FrameClose, FrameErr:
		raw, err := unb64u(f.B)
		if err != nil {
			return "", nil, fmt.Errorf("%w: bad b64", ErrFrame)
		}
		pt, err := c.aead.Open(nil, nonceFor(f.N), raw, nil)
		if err != nil {
			return "", nil, ErrDecrypt
		}
		return f.T, pt, nil
	default:
		return "", nil, fmt.Errorf("%w: type %q", ErrFrame, f.T)
	}
}
