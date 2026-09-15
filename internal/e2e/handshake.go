package e2e

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// helloEnvelope / respondEnvelope 是握手明文信封（经 relay 转发，relay 可见元数据）。
// 序列化规则（两端必须一致）：Go json.Marshal 按字段定义顺序、紧凑分隔符。
// 注意：若未来出现非 Go 实现（如 Swift），需按同一字段顺序构造 JSON——见 PROTOCOL.md。
type helloEnvelope struct {
	V        int    `json:"v"`
	T        string `json:"t"` // "hello"
	DeviceID string `json:"device_id"`
	EDev     string `json:"e_dev"` // base64url 无填充，X25519 临时公钥
	TS       int64  `json:"ts"`    // unix 秒，新鲜性窗口 ±MaxClockSkewSec
}

type respondEnvelope struct {
	V    int    `json:"v"`
	T    string `json:"t"` // "respond"
	EPsn string `json:"e_psn"`
	TS   int64  `json:"ts"`
}

func b64u(b []byte) string            { return base64.RawURLEncoding.EncodeToString(b) }
func unb64u(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// hkdfSHA256 一步到位派生 length 字节密钥材料（Go 1.24+ 标准库 crypto/hkdf）。
func hkdfSHA256(ikm, salt, info []byte, length int) ([]byte, error) {
	return hkdf.Key(sha256.New, ikm, salt, string(info), length)
}

func parsePub(b64 string) (*ecdh.PublicKey, error) {
	raw, err := unb64u(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: bad base64 pubkey", ErrBadEnvelope)
	}
	pub, err := x25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: bad x25519 pubkey: %v", ErrBadEnvelope, err)
	}
	return pub, nil
}

func freshTS(ts int64, now time.Time) bool {
	d := now.Unix() - ts
	return d <= MaxClockSkewSec && d >= -MaxClockSkewSec
}

// Initiator 是握手发起方（app 侧）。
type Initiator struct {
	LongTerm  *ecdh.PrivateKey // 设备长期私钥
	PeerLong  *ecdh.PublicKey  // daemon 长期公钥（配对时经 QR 获得）
	Ephemeral *ecdh.PrivateKey // 本连接临时私钥
	DeviceID  string
	Now       func() time.Time // 可注入时钟（测试/向量生成）；nil = time.Now

	hello []byte // 缓存原文，Finish 时算 transcript
}

// NewInitiator 组装发起方；临时密钥由调用方生成（每次隧道一次，保证前向安全）。
func NewInitiator(deviceID string, longTerm, ephemeral *ecdh.PrivateKey, peerLong *ecdh.PublicKey) *Initiator {
	return &Initiator{LongTerm: longTerm, PeerLong: peerLong, Ephemeral: ephemeral, DeviceID: deviceID}
}

func (i *Initiator) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// Hello 构造并缓存 hello 信封（发给 relay，转给 daemon）。
func (i *Initiator) Hello() ([]byte, error) {
	env := helloEnvelope{
		V:        1,
		T:        "hello",
		DeviceID: i.DeviceID,
		EDev:     b64u(i.Ephemeral.PublicKey().Bytes()),
		TS:       i.now().Unix(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshal hello: %w", err)
	}
	i.hello = data
	return data, nil
}

// Finish 处理 respond 信封，验证新鲜性并派生方向密钥。
func (i *Initiator) Finish(respond []byte) (*Keys, error) {
	var env respondEnvelope
	if err := json.Unmarshal(respond, &env); err != nil {
		return nil, fmt.Errorf("%w: respond: %v", ErrBadEnvelope, err)
	}
	if env.V != 1 || env.T != "respond" {
		return nil, fmt.Errorf("%w: respond type/v", ErrBadEnvelope)
	}
	if !freshTS(env.TS, i.now()) {
		return nil, ErrStaleTS
	}
	ePsn, err := parsePub(env.EPsn)
	if err != nil {
		return nil, err
	}
	s1, err := mustShared(i.LongTerm, i.PeerLong) // dev_long × psn_long
	if err != nil {
		return nil, err
	}
	s2, err := mustShared(i.Ephemeral, i.PeerLong) // e_dev × psn_long
	if err != nil {
		return nil, err
	}
	s3, err := mustShared(i.LongTerm, ePsn) // dev_long × e_psn
	if err != nil {
		return nil, err
	}
	return derive(s1, s2, s3, transcript(i.hello, respond), true)
}

// Responder 是握手响应方（daemon 侧）。
type Responder struct {
	LongTerm  *ecdh.PrivateKey // daemon 长期私钥
	PeerLong  *ecdh.PublicKey  // 设备长期公钥（配对时存入）
	Ephemeral *ecdh.PrivateKey // 本连接临时私钥
	Now       func() time.Time // 可注入时钟；nil = time.Now
}

func NewResponder(longTerm, ephemeral *ecdh.PrivateKey, peerLong *ecdh.PublicKey) *Responder {
	return &Responder{LongTerm: longTerm, PeerLong: peerLong, Ephemeral: ephemeral}
}

func (r *Responder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Respond 处理 hello 信封，返回 respond 原文、派生密钥、声明的设备 ID。
// deviceID 由调用方对照设备库（未配对设备在此拒绝）。
func (r *Responder) Respond(hello []byte) (respond []byte, keys *Keys, deviceID string, err error) {
	var env helloEnvelope
	if err := json.Unmarshal(hello, &env); err != nil {
		return nil, nil, "", fmt.Errorf("%w: hello: %v", ErrBadEnvelope, err)
	}
	if env.V != 1 || env.T != "hello" || env.DeviceID == "" {
		return nil, nil, "", fmt.Errorf("%w: hello type/v/device", ErrBadEnvelope)
	}
	if !freshTS(env.TS, r.now()) {
		return nil, nil, "", ErrStaleTS
	}
	eDev, err := parsePub(env.EDev)
	if err != nil {
		return nil, nil, "", err
	}
	s1, err := mustShared(r.LongTerm, r.PeerLong) // psn_long × dev_long
	if err != nil {
		return nil, nil, "", err
	}
	s2, err := mustShared(r.LongTerm, eDev) // psn_long × e_dev
	if err != nil {
		return nil, nil, "", err
	}
	s3, err := mustShared(r.Ephemeral, r.PeerLong) // e_psn × dev_long
	if err != nil {
		return nil, nil, "", err
	}
	respEnv := respondEnvelope{
		V:    1,
		T:    "respond",
		EPsn: b64u(r.Ephemeral.PublicKey().Bytes()),
		TS:   r.now().Unix(),
	}
	respond, err = json.Marshal(respEnv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("marshal respond: %w", err)
	}
	keys, err = derive(s1, s2, s3, transcript(hello, respond), false)
	if err != nil {
		return nil, nil, "", err
	}
	return respond, keys, env.DeviceID, nil
}
