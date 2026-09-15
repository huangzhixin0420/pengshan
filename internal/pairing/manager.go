// Package pairing 实现扫码配对：终端生成一次性配对会话 + QR 载荷，
// app 在 LAN 直连下 POST claim（提交设备公钥），daemon 验码注册设备。
// 协议（DEV-PLAN §4.2）：
//
//	QR 载荷 = {v, kind:"pengshan-pair", claim_urls, psn_id, psn_pk, session_id, code, exp}
//	claim   = POST <claim_url> {session_id, code, device_id, label, platform, pub_key}
//
// M2 走 LAN 直连 claim（扫码场景手机与 Mac 同网段；hermes-link preferred_urls 同款）。
// relay 中转配对（远程配对、无 LAN）在 M3 随 control 信令通道补：
// 届时 claim_urls 里放 relay 地址，app 无需改动。
//
// 安全要点：
//   - code 8 位 Crockford Base32（无 0/O/1/I），10 分钟过期，一次性，原子 claim；
//   - 未 claim 会话数上限（防刷配对窗口）；
//   - pub_key 32B X25519 严格校验。
package pairing

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/huangzhixin0420/pengshan/internal/storage"
)

const (
	// CodeTTL 是配对窗口。
	CodeTTL = 10 * time.Minute
	// MaxPending 是同时存活的未 claim 会话上限。
	MaxPending = 4
	// CodeLength 是配对码位数。
	CodeLength = 8

	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford Base32
)

var (
	// ErrTooManyPending 等业务错误。
	ErrTooManyPending = errors.New("pairing: too many pending sessions")
)

// QRPayload 是 QR 码内容（JSON）。
type QRPayload struct {
	V        int      `json:"v"`
	Kind     string   `json:"kind"` // "pengshan-pair"
	ClaimURLs []string `json:"claim_urls"` // LAN 直连 claim 端点，优先第一个可达
	PsnID    string   `json:"psn_id"`
	PsnPK    string   `json:"psn_pk"` // base64url X25519 32B
	SessionID string  `json:"session_id"`
	Code     string   `json:"code"`
	Exp      int64    `json:"exp"` // unix 秒
}

// ClaimRequest 是 app 提交的 claim 体。
type ClaimRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
	DeviceID  string `json:"device_id"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	PubKey    string `json:"pub_key"` // base64url X25519 32B
}

// ClaimResponse 是 claim 的响应（含 daemon 身份信息，app 存下供握手用）。
type ClaimResponse struct {
	OK      bool   `json:"ok"`
	DeviceID string `json:"device_id,omitempty"`
	PsnID   string `json:"psn_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Manager 组织配对流程。
type Manager struct {
	store  *storage.Store
	psnID  string
	psnPub *ecdh.PublicKey
	now    func() time.Time
}

// NewManager 组装；psnPub 用于 QR 载荷（app 验握手 transcript）。
func NewManager(store *storage.Store, psnID string, psnPub *ecdh.PublicKey) *Manager {
	return &Manager{store: store, psnID: psnID, psnPub: psnPub, now: time.Now}
}

// Now 可注入时钟（测试）。
func (m *Manager) Now(f func() time.Time) { m.now = f }

func generateCode() (string, error) {
	buf := make([]byte, CodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, CodeLength)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

// CreateSession 建配对会话并返回 QR 载荷（含 code）。
// claimURLs 由调用方（CLI）探测 LAN 地址后传入。
func (m *Manager) CreateSession(claimURLs []string, note string) (*storage.PairingSession, *QRPayload, error) {
	now := m.now()
	n, err := m.store.CountPendingPairings(now)
	if err != nil {
		return nil, nil, err
	}
	if n >= MaxPending {
		return nil, nil, ErrTooManyPending
	}
	code, err := generateCode()
	if err != nil {
		return nil, nil, err
	}
	sess := storage.PairingSession{
		SessionID: fmt.Sprintf("pair_%d", now.UnixNano()),
		Code:      code,
		ExpiresAt: now.Add(CodeTTL),
		Note:      note,
		CreatedAt: now,
	}
	if err := m.store.CreatePairingSession(sess); err != nil {
		return nil, nil, err
	}
	_ = m.store.AppendAudit(now, "local_cli", "", "pairing.session.created",
		map[string]string{"session_id": sess.SessionID})

	qr := &QRPayload{
		V:         1,
		Kind:      "pengshan-pair",
		ClaimURLs: claimURLs,
		PsnID:     m.psnID,
		PsnPK:     base64.RawURLEncoding.EncodeToString(m.psnPub.Bytes()),
		SessionID: sess.SessionID,
		Code:      code,
		Exp:       sess.ExpiresAt.Unix(),
	}
	return &sess, qr, nil
}

// Claim 处理 app 的 claim：原子验码 → 注册设备 → 审计。
func (m *Manager) Claim(req ClaimRequest) (*storage.Device, error) {
	now := m.now()
	if req.SessionID == "" || req.Code == "" || req.DeviceID == "" || req.PubKey == "" {
		return nil, errors.New("pairing: session_id/code/device_id/pub_key required")
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(req.PubKey)
	if err != nil || len(pubRaw) != 32 {
		return nil, errors.New("pairing: bad pub_key")
	}
	if _, err := ecdh.X25519().NewPublicKey(pubRaw); err != nil {
		return nil, fmt.Errorf("pairing: pub_key not X25519: %w", err)
	}

	sess, err := m.store.ClaimPairingSession(req.SessionID, normalizeCode(req.Code), req.DeviceID, now)
	if err != nil {
		return nil, err
	}
	dev := storage.Device{
		DeviceID: req.DeviceID,
		Label:    req.Label,
		Platform: req.Platform,
		PubKey:   pubRaw,
		PairedAt: now,
		LastSeen: now,
	}
	if err := m.store.PutDevice(dev); err != nil {
		return nil, err
	}
	_ = m.store.AppendAudit(now, "remote_device", req.DeviceID, "pairing.session.claimed",
		map[string]string{"session_id": sess.SessionID, "label": req.Label, "platform": req.Platform})
	return &dev, nil
}

// normalizeCode 大写去空白/连字符（手输容错；Crockford 允许分组）。
func normalizeCode(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		out = append(out, c)
	}
	return string(out)
}

// RenderQR 把 QR 载荷渲染为终端 ASCII 二维码（half-block）。
func RenderQR(payload *QRPayload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return renderQRASCII(string(data))
}
