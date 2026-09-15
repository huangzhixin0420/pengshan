package e2e

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// vector 是固定密钥材料派生的握手向量，供第三方复算（DEV-PLAN §9）。
type vector struct {
	Name        string `json:"name"`
	DevLongSeed string `json:"dev_long_seed"` // hex，32B
	PsnLongSeed string `json:"psn_long_seed"`
	DevEphSeed  string `json:"dev_ephemeral_seed"`
	PsnEphSeed  string `json:"psn_ephemeral_seed"`
	DeviceID    string `json:"device_id"`

	Hello      string `json:"hello_b64"`
	Respond    string `json:"respond_b64"`
	AppSendKey string `json:"app_send_key_hex"`
	AppRecvKey string `json:"app_recv_key_hex"`
}

//go:embed testdata/handshake-vector.json
var vectorJSON []byte

func seedKey(t *testing.T, seedHex string) *ecdh.PrivateKey {
	t.Helper()
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != 32 {
		t.Fatalf("bad seed: %v", err)
	}
	priv, err := x25519().NewPrivateKey(seed)
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	return priv
}

func loadVector(t *testing.T) vector {
	t.Helper()
	var v vector
	if err := json.Unmarshal(vectorJSON, &v); err != nil {
		t.Fatalf("parse vector: %v", err)
	}
	return v
}

// TestHandshake_Vector 用固定种子复算握手：钉死时钟后逐字节比对 hello/respond，
// 并断言派生 key 与向量一致。第三方可用 testdata/handshake-vector.json 独立复算
// （E2E 可审计的兑现；重新生成：PENGSHAN_UPDATE_VECTOR=1 go test ./internal/e2e/）。
func TestHandshake_Vector(t *testing.T) {
	v := loadVector(t)
	clock := func() time.Time { return time.Unix(vecClockUnix, 0) }

	devLong := seedKey(t, v.DevLongSeed)
	psnLong := seedKey(t, v.PsnLongSeed)
	devEph := seedKey(t, v.DevEphSeed)
	psnEph := seedKey(t, v.PsnEphSeed)

	init := NewInitiator(v.DeviceID, devLong, devEph, psnLong.PublicKey())
	init.Now = clock
	hello, err := init.Hello()
	if err != nil {
		t.Fatal(err)
	}
	gotHello := b64u(hello)
	if gotHello != v.Hello {
		t.Fatalf("hello mismatch:\n got %s\nwant %s", gotHello, v.Hello)
	}

	resp := NewResponder(psnLong, psnEph, devLong.PublicKey())
	resp.Now = clock
	respond, rk, deviceID, err := resp.Respond(hello)
	if err != nil {
		t.Fatal(err)
	}
	if deviceID != v.DeviceID {
		t.Fatalf("deviceID = %q", deviceID)
	}
	gotRespond := b64u(respond)
	if gotRespond != v.Respond {
		t.Fatalf("respond mismatch:\n got %s\nwant %s", gotRespond, v.Respond)
	}

	ik, err := init.Finish(respond)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ik.Send[:]); got != v.AppSendKey {
		t.Fatalf("app send key mismatch:\n got %s\nwant %s", got, v.AppSendKey)
	}
	if got := hex.EncodeToString(ik.Recv[:]); got != v.AppRecvKey {
		t.Fatalf("app recv key mismatch:\n got %s\nwant %s", got, v.AppRecvKey)
	}
	if ik.Send != rk.Recv || ik.Recv != rk.Send {
		t.Fatal("keys not mirrored between initiator/responder")
	}
}

// TestHandshake_RoundTrip 全随机材料：两端握手，验证方向密钥互反、
// 帧加解密往返、重放/跳变/篡改拒绝。
func TestHandshake_RoundTrip(t *testing.T) {
	for i := 0; i < 20; i++ {
		devLong := newKey(t)
		psnLong := newKey(t)
		devEph := newKey(t)
		psnEph := newKey(t)

		init := NewInitiator("dev_test", devLong, devEph, psnLong.PublicKey())
		hello, err := init.Hello()
		if err != nil {
			t.Fatal(err)
		}

		resp := NewResponder(psnLong, psnEph, devLong.PublicKey())
		respond, rk, deviceID, err := resp.Respond(hello)
		if err != nil {
			t.Fatalf("respond: %v", err)
		}
		if deviceID != "dev_test" {
			t.Fatalf("deviceID = %q", deviceID)
		}

		ik, err := init.Finish(respond)
		if err != nil {
			t.Fatalf("finish: %v", err)
		}

		// 方向互反：app.Send == daemon.Recv，反之亦然。
		if ik.Send != rk.Recv {
			t.Fatal("send/recv mismatch (app.Send != daemon.Recv)")
		}
		if ik.Recv != rk.Send {
			t.Fatal("send/recv mismatch (app.Recv != daemon.Send)")
		}

		// 帧往返：app 封 → daemon 解。
		as, ar := NewCipher(ik.Send, true), NewCipher(rk.Recv, false)
		msg := bytes.Repeat([]byte("pengshan-"), 100)
		sealed, err := as.Seal(FrameData, msg)
		if err != nil {
			t.Fatal(err)
		}
		ft, pt, err := ar.Open(sealed)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if ft != FrameData || !bytes.Equal(pt, msg) {
			t.Fatal("roundtrip mismatch")
		}

		// ping 帧不占密文但占序号。
		pf, err := as.Seal(FramePing, nil)
		if err != nil {
			t.Fatal(err)
		}
		ft, _, err = ar.Open(pf)
		if err != nil || ft != FramePing {
			t.Fatalf("ping: %v %q", err, ft)
		}
	}
}

func TestCipher_ReplayAndTamper(t *testing.T) {
	key := [32]byte{1, 2, 3}
	s, r := NewCipher(key, true), NewCipher(key, false)

	f1, _ := s.Seal(FrameData, []byte("one"))
	f2, _ := s.Seal(FrameData, []byte("two"))
	f3, _ := s.Seal(FrameData, []byte("three"))

	if _, _, err := r.Open(f1); err != nil {
		t.Fatal(err)
	}
	// 重放 f1：序号回退 → 拒绝。
	if _, _, err := r.Open(f1); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay accepted: %v", err)
	}
	// 跳帧（f3 直接来）：拒绝。
	if _, _, err := r.Open(f3); !errors.Is(err, ErrReplay) {
		t.Fatalf("jump accepted: %v", err)
	}
	if _, _, err := r.Open(f2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Open(f3); err != nil {
		t.Fatal(err)
	}

	// 篡改密文：改 b 中间一个字节 → 解密失败。
	f4, _ := s.Seal(FrameData, []byte("four"))
	var tampered Frame
	if err := json.Unmarshal(f4, &tampered); err != nil {
		t.Fatal(err)
	}
	raw, _ := unb64u(tampered.B)
	raw[len(raw)/2] ^= 0xFF
	tampered.B = b64u(raw)
	bad, _ := json.Marshal(tampered)
	if _, _, err := r.Open(bad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tamper accepted: %v", err)
	}
}

func TestHandshake_StaleTimestamp(t *testing.T) {
	devLong, psnLong := newKey(t), newKey(t)
	devEph, psnEph := newKey(t), newKey(t)
	now := time.Now()
	clock := func() time.Time { return now }

	init := NewInitiator("d", devLong, devEph, psnLong.PublicKey())
	init.Now = clock
	hello, err := init.Hello()
	if err != nil {
		t.Fatal(err)
	}
	resp := NewResponder(psnLong, psnEph, devLong.PublicKey())
	resp.Now = clock
	respond, _, _, err := resp.Respond(hello)
	if err != nil {
		t.Fatal(err)
	}
	// 时间推进超出窗口后再 Finish。
	later := now.Add(MaxClockSkewSec*time.Second + time.Second)
	init.Now = func() time.Time { return later }
	if _, err := init.Finish(respond); !errors.Is(err, ErrStaleTS) {
		t.Fatalf("stale accepted: %v", err)
	}
}

func TestHandshake_WrongPeerKey(t *testing.T) {
	// daemon 用错误的设备公钥（中间人没有真设备私钥的等价场景）：
	// 握手完成但派生 key 与 app 不一致 → 首帧解密失败。
	devLong, psnLong := newKey(t), newKey(t)
	devEph, psnEph := newKey(t), newKey(t)
	fakeDevLong := newKey(t)

	init := NewInitiator("d", devLong, devEph, psnLong.PublicKey())
	hello, _ := init.Hello()
	resp := NewResponder(psnLong, psnEph, fakeDevLong.PublicKey())
	respond, rk, _, err := resp.Respond(hello)
	if err != nil {
		t.Fatal(err)
	}
	ik, err := init.Finish(respond)
	if err != nil {
		t.Fatal(err)
	}
	if ik.Send == rk.Recv {
		t.Fatal("wrong-peer keys should diverge")
	}
	f, _ := NewCipher(ik.Send, true).Seal(FrameData, []byte("hi"))
	if _, _, err := NewCipher(rk.Recv, false).Open(f); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong-peer frame opened: %v", err)
	}
}

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := x25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// FuzzOpen：随机输入不得 panic、不得推进序号（要么报错要么完整打开）。
func FuzzOpen(f *testing.F) {
	key := [32]byte{9}
	s := NewCipher(key, true)
	good, _ := s.Seal(FrameData, []byte("seed"))
	f.Add(good)
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewCipher(key, false)
		_, _, _ = r.Open(data) // 断言隐含：无 panic、无 data race（-race 跑）
	})
}

// FuzzHandshakeEnvelope：随机 hello/respond 字节不得 panic。
func FuzzHandshakeEnvelope(f *testing.F) {
	devLong := mustKeySeed(f)
	psnLong := mustKeySeed(f)
	init := NewInitiator("d", devLong, mustKeySeed(f), psnLong.PublicKey())
	hello, _ := init.Hello()
	f.Add(hello)
	f.Add([]byte(`{"v":1,"t":"hello","device_id":"x","e_dev":"AAAA","ts":1700000000}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		resp := NewResponder(psnLong, newKey(t), devLong.PublicKey())
		respond, _, _, err := resp.Respond(data)
		if err != nil {
			return
		}
		// 能 respond 的（概率近零）也不允许 Finish 出事。
		init2 := NewInitiator("d", devLong, mustKeySeed(t), psnLong.PublicKey())
		init2.hello = data
		_, _ = init2.Finish(respond)
	})
}

// mustKeySeed 是 fuzz 友好的密钥生成（testing.T 零值不能过 Helper 路径）。
func mustKeySeed(tb testing.TB) *ecdh.PrivateKey {
	tb.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	return k
}

// TestMain 支持 -update-vector：重新生成 testdata/handshake-vector.json。
// 生成器实现在 vector_generate_test.go（单独文件，避免常规测试引用 time 注入）。
func TestMain(m *testing.M) {
	if os.Getenv("PENGSHAN_UPDATE_VECTOR") == "1" {
		if err := writeVector(filepath.Join("testdata", "handshake-vector.json")); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
