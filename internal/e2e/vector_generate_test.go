package e2e

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 固定向量种子（公开、可复算；不是真实密钥材料）。
// 变更握手协议时必须重新生成并记录变更理由。
const (
	vecDevLongSeed = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	vecPsnLongSeed = "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"
	vecDevEphSeed  = "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"
	vecPsnEphSeed  = "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f"
	vecDeviceID    = "dev_vector0001"
	vecClockUnix   = 1_700_000_000
)

// writeVector 以固定时钟/种子重放握手，把 hello/respond/派生 key 写入 path。
// 运行：PENGSHAN_UPDATE_VECTOR=1 go test ./internal/e2e/
func writeVector(path string) error {
	mustKey := func(seed string) *ecdh.PrivateKey {
		raw, err := hex.DecodeString(seed)
		if err != nil {
			panic(err)
		}
		priv, err := ecdh.X25519().NewPrivateKey(raw)
		if err != nil {
			panic(err)
		}
		return priv
	}
	clock := func() time.Time { return time.Unix(vecClockUnix, 0) }

	devLong := mustKey(vecDevLongSeed)
	psnLong := mustKey(vecPsnLongSeed)
	devEph := mustKey(vecDevEphSeed)
	psnEph := mustKey(vecPsnEphSeed)

	init := NewInitiator(vecDeviceID, devLong, devEph, psnLong.PublicKey())
	init.Now = clock
	hello, err := init.Hello()
	if err != nil {
		return err
	}

	resp := NewResponder(psnLong, psnEph, devLong.PublicKey())
	resp.Now = clock
	respond, rk, deviceID, err := resp.Respond(hello)
	if err != nil {
		return err
	}
	if deviceID != vecDeviceID {
		panic("vector device mismatch")
	}

	ik, err := init.Finish(respond)
	if err != nil {
		return err
	}
	if ik.Send != rk.Recv || ik.Recv != rk.Send {
		panic("vector keys not mirrored")
	}

	v := vector{
		Name:        "pengshan-tunnel-handshake-v1",
		DevLongSeed: vecDevLongSeed,
		PsnLongSeed: vecPsnLongSeed,
		DevEphSeed:  vecDevEphSeed,
		PsnEphSeed:  vecPsnEphSeed,
		DeviceID:    vecDeviceID,
		Hello:       base64.RawURLEncoding.EncodeToString(hello),
		Respond:     base64.RawURLEncoding.EncodeToString(respond),
		AppSendKey:  hex.EncodeToString(ik.Send[:]),
		AppRecvKey:  hex.EncodeToString(ik.Recv[:]),
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
