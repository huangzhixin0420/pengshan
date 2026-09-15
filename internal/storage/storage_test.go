package storage

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDeviceLifecycle(t *testing.T) {
	s := openTemp(t)
	now := time.Now()

	d := Device{DeviceID: "dev_1", Label: "Leo 的 iPhone", Platform: "ios", PubKey: make([]byte, 32), PairedAt: now, LastSeen: now}
	if err := s.PutDevice(d); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetDevice("dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != d.Label || string(got.PubKey) != string(d.PubKey) {
		t.Fatal("roundtrip mismatch")
	}

	devs, err := s.ListDevices()
	if err != nil || len(devs) != 1 {
		t.Fatalf("list: %v %d", err, len(devs))
	}

	if err := s.RevokeDevice("dev_1", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDevice("dev_1"); err != ErrDeviceRevoked {
		t.Fatalf("want revoked, got %v", err)
	}
	if _, err := s.GetDevicePubKey("dev_1"); err != ErrDeviceRevoked {
		t.Fatalf("want revoked (pubkey), got %v", err)
	}
	devs, _ = s.ListDevices()
	if len(devs) != 0 {
		t.Fatalf("revoked device listed: %d", len(devs))
	}

	// 再 claim 同 ID = 复活（覆盖）。
	if err := s.PutDevice(d); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDevice("dev_1"); err != nil {
		t.Fatalf("re-pair: %v", err)
	}
}

func TestPairingClaimAtomic(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	p := PairingSession{
		SessionID: "pair_x", Code: "ABCD2345", ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}
	if err := s.CreatePairingSession(p); err != nil {
		t.Fatal(err)
	}

	// 并发 16 个 claim，只一个成功。
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.ClaimPairingSession("pair_x", "ABCD2345", "dev_"+string(rune('a'+i)), now.Add(time.Second))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("concurrent claims succeeded = %d, want 1", ok)
	}

	// 已 claim → 再 claim 报 claimed。
	if _, err := s.ClaimPairingSession("pair_x", "ABCD2345", "dev_z", now.Add(time.Second)); err != ErrPairingClaimed {
		t.Fatalf("want claimed, got %v", err)
	}
}

func TestPairingExpired(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	p := PairingSession{
		SessionID: "pair_e", Code: "EEEEEEEE", ExpiresAt: now.Add(-time.Second), CreatedAt: now.Add(-time.Minute),
	}
	if err := s.CreatePairingSession(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPairingSession("pair_e", "EEEEEEEE", "dev", now); err != ErrPairingExpired {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestPairingNotFound(t *testing.T) {
	s := openTemp(t)
	if _, err := s.ClaimPairingSession("nope", "XXXXXXXX", "dev", time.Now()); err != ErrPairingNotFound {
		t.Fatalf("want notfound, got %v", err)
	}
}

func TestAuditAndMeta(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	if err := s.AppendAudit(now, "local_cli", "", "test.event", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta("signing_secret", "abc"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetMeta("signing_secret"); v != "abc" {
		t.Fatalf("meta = %q", v)
	}
	if v, _ := s.GetMeta("missing"); v != "" {
		t.Fatalf("missing meta = %q", v)
	}
}
