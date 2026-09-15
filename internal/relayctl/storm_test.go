package relayctl_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
	"github.com/huangzhixin0420/pengshan/internal/relayclient"
	"github.com/huangzhixin0420/pengshan/internal/relayctl"
	"github.com/huangzhixin0420/pengshan/internal/tunnel"
)

// TestRelayStorm：100 并发客户端 × 每轮完整握手+echo 1KB+主动断开，
// 验证 relay 不崩、无错误、无 goroutine 泄漏。
// 这是 DEV-PLAN §8 M5 的"重连风暴压测"自动化版（一轮 = 一次 attach 循环）。
func TestRelayStorm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	srv.SetAttachTimeoutForTest(2 * time.Second)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	devLong, psnLong := newKey(t), newKey(t)
	dh := &daemonHandle{priv: psnLong, devPub: devLong.PublicKey()}
	startDaemon(t, baseURL, dh, "echo")

	const clients = 100
	const rounds = 3

	before := runtime.NumGoroutine()
	start := time.Now()

	var wg sync.WaitGroup
	errs := make(chan error, clients*rounds)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if err := stormRound(baseURL, devLong, psnLong.PublicKey()); err != nil {
					errs <- fmt.Errorf("client %d round %d: %w", c, r, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)

	for err := range errs {
		t.Fatal(err)
	}

	// goroutine 泄漏检查：泵与对接循环应全部退出。
	// 清理时序 = attachTimeout（2s）+ 余量，窗口对齐否则误报。
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	t.Logf("storm: %d clients × %d rounds in %v; goroutines %d → %d", clients, rounds, elapsed.Round(time.Millisecond), before, after)
	if after > before+4 {
		// 打印泄漏 goroutine 的栈（前 12 个）定位。
		buf := make([]byte, 2<<20)
		n := runtime.Stack(buf, true)
		stacks := string(buf[:n])
		lines := strings.Split(stacks, "\n\n")
		counts := map[string]int{}
		matched := 0
		for _, blk := range lines {
			hit := false
			for _, key := range []string{"handleTunnel", "handleAttach", "pumpConn", "readLoop", "io.Copy", "attachLoop", "runEchoDaemon", "stormRound", "Read("} {
				if strings.Contains(blk, key) {
					counts[key]++
					hit = true
					break
				}
			}
			if !hit {
				counts["<other>"]++
				if counts["<other>"] == 1 {
					t.Logf("other sample: %.300s", blk)
				}
			}
			matched++
		}
		for k, v := range counts {
			t.Logf("leak: %s × %d", k, v)
		}
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

// stormRound 一次完整隧道生命周期：dial → 握手 → echo 1KB → 关闭。
func stormRound(baseURL string, devLong *ecdh.PrivateKey, psnPub *ecdh.PublicKey) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wsConn, _, err := websocket.Dial(ctx, baseURL+"/tunnel", nil)
	if err != nil {
		return err
	}
	wsConn.SetReadLimit(1 << 20)
	init := e2e.NewInitiator("dev_test", devLong, mustEphKey(), psnPub)
	hello, err := init.Hello()
	if err != nil {
		return err
	}
	if err := wsConn.Write(ctx, websocket.MessageText, hello); err != nil {
		return err
	}
	_, respond, err := wsConn.Read(ctx)
	if err != nil {
		return err
	}
	keys, err := init.Finish(respond)
	if err != nil {
		return err
	}
	ep := tunnel.New(wsConn, keys)
	defer ep.Close()

	want := make([]byte, 1024)
	rand.Read(want)
	writeDone := make(chan error, 1)
	go func() {
		_, err := ep.Write(want)
		writeDone <- err
	}()
	got := make([]byte, 1024)
	if _, err := io.ReadFull(ep, got); err != nil {
		return err
	}
	if err := <-writeDone; err != nil {
		return err
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("echo mismatch")
		}
	}
	return nil
}

func mustEphKey() *ecdh.PrivateKey {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

// TestTopologySnapshot：daemon 上报 → /api/v1/topology 可查。
func TestTopologySnapshot(t *testing.T) {
	srv := relayctl.NewServer(nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	ctrl, err := relayclient.DialControl(context.Background(), baseURL, "psn_topo")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	time.Sleep(100 * time.Millisecond)

	err = ctrl.SendTopology(relayclient.Topology{
		LANEndpoints: []string{"192.168.1.10"},
		ServeOK:      true,
		ServeAddr:    "127.0.0.1:9121",
		Version:      "test",
		Devices:      3,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	resp, err := http.Get(ts.URL + "/api/v1/topology")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("topology HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{`"serve_ok":true`, `"192.168.1.10"`, `"devices":3`, `"version":"test"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("topology 缺 %s: %s", want, s)
		}
	}
}
