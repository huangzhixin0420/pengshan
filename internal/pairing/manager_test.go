package pairing

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzhixin0420/pengshan/internal/storage"
)

func newTestManager(t *testing.T) (*Manager, *storage.Store) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(store, "psn_test", priv.PublicKey()), store
}

func TestCreateAndClaim(t *testing.T) {
	mgr, store := newTestManager(t)

	sess, qr, err := mgr.CreateSession([]string{"http://192.168.1.10:9517/pairing/claim"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Code) != CodeLength {
		t.Fatalf("code len %d", len(sess.Code))
	}
	if qr.Kind != "pengshan-pair" || qr.SessionID != sess.SessionID || qr.PsnPK == "" {
		t.Fatal("qr payload incomplete")
	}

	// QR 可序列化且含 claim URL。
	raw, err := json.Marshal(qr)
	if err != nil || !strings.Contains(string(raw), "claim") {
		t.Fatalf("qr json: %v", err)
	}

	// 设备密钥。
	devKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	claimed, err := mgr.Claim(ClaimRequest{
		SessionID: sess.SessionID,
		Code:      strings.ToLower(sess.Code[:4]) + "-" + sess.Code[4:], // 小写+连字符容错
		DeviceID:  "dev_iphone",
		Label:     "Leo 的 iPhone",
		Platform:  "ios",
		PubKey:    base64.RawURLEncoding.EncodeToString(devKey.PublicKey().Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.DeviceID != "dev_iphone" {
		t.Fatal("device mismatch")
	}

	// 设备已入库可验。
	got, err := store.GetDevice("dev_iphone")
	if err != nil || len(got.PubKey) != 32 {
		t.Fatalf("stored device: %v", err)
	}

	// 重放 claim 被拒。
	if _, err := mgr.Claim(ClaimRequest{
		SessionID: sess.SessionID, Code: sess.Code, DeviceID: "dev_evil",
		PubKey: base64.RawURLEncoding.EncodeToString(devKey.PublicKey().Bytes()),
	}); err == nil {
		t.Fatal("replay claim accepted")
	}
}

func TestClaimBadInputs(t *testing.T) {
	mgr, _ := newTestManager(t)
	sess, _, _ := mgr.CreateSession(nil, "")

	cases := []ClaimRequest{
		{SessionID: sess.SessionID, Code: sess.Code, DeviceID: "d", PubKey: "not-base64!!"},
		{SessionID: sess.SessionID, Code: sess.Code, DeviceID: "d", PubKey: base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		{SessionID: sess.SessionID, Code: "WRONGCOD", DeviceID: "d", PubKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
	}
	for i, req := range cases {
		if _, err := mgr.Claim(req); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestMaxPending(t *testing.T) {
	mgr, _ := newTestManager(t)
	mgr.Now(func() time.Time { return time.Now() }) // 默认时钟
	for i := 0; i < MaxPending; i++ {
		if _, _, err := mgr.CreateSession(nil, ""); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, _, err := mgr.CreateSession(nil, ""); err != ErrTooManyPending {
		t.Fatalf("want too-many, got %v", err)
	}
}

func TestRenderQR(t *testing.T) {
	mgr, _ := newTestManager(t)
	_, qr, _ := mgr.CreateSession([]string{"http://127.0.0.1:1/claim"}, "")
	out, err := RenderQR(qr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "█") {
		t.Fatal("qr ascii empty")
	}
}
