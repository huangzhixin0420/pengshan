// M1 集成验收（DEV-PLAN §8 M1）：relay + daemon(echo) + app 三角色，
// 经真实 WS 对接与 e2e 握手，验证隧道字节流 64KB/1MB/64MB 往返无损。
// ch 由 Registry 直注（M2 起由配对/control open 路径产生）。
package relayctl_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
	"github.com/huangzhixin0420/pengshan/internal/relayclient"
	"github.com/huangzhixin0420/pengshan/internal/relayctl"
	"github.com/huangzhixin0420/pengshan/internal/tunnel"
)

// runEchoDaemon 模拟 daemon 侧：attach 到 ch → 完成 e2e 握手 → 隧道字节自回显。
func runEchoDaemon(t *testing.T, baseURL, ch string, psnLong *ecdh.PrivateKey, devPub *ecdh.PublicKey) {
	t.Helper()
	ctx := context.Background()

	wsConn, err := relayclient.Attach(ctx, baseURL, ch)
	if err != nil {
		t.Errorf("daemon attach: %v", err)
		return
	}
	defer wsConn.CloseNow()

	// 1) 读 hello
	_, hello, err := wsConn.Read(ctx)
	if err != nil {
		t.Errorf("daemon read hello: %v", err)
		return
	}
	resp := e2e.NewResponder(psnLong, newEphKey(t), devPub)
	respond, keys, _, err := resp.Respond(hello)
	if err != nil {
		t.Errorf("respond: %v", err)
		return
	}
	if err := wsConn.Write(ctx, websocket.MessageText, respond); err != nil {
		t.Errorf("daemon write respond: %v", err)
		return
	}

	// 2) 隧道自回显
	ep := tunnel.New(wsConn, keys)
	defer ep.Close()
	_, _ = io.Copy(ep, ep)
}

// dialTunnel 模拟 app 侧：连 /tunnel/{ch} → 发 hello → 收 respond → 返回隧道端点。
func dialTunnel(t *testing.T, baseURL, ch string, devLong *ecdh.PrivateKey, psnPub *ecdh.PublicKey) *tunnel.Endpoint {
	t.Helper()
	ctx := context.Background()

	wsConn, _, err := websocket.Dial(ctx, fmt.Sprintf("%s/tunnel/%s", baseURL, ch), nil)
	if err != nil {
		t.Fatalf("app dial tunnel: %v", err)
	}

	init := e2e.NewInitiator("dev_test", devLong, newEphKey(t), psnPub)
	hello, err := init.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := wsConn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("app write hello: %v", err)
	}
	_, respond, err := wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("app read respond: %v", err)
	}
	keys, err := init.Finish(respond)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return tunnel.New(wsConn, keys)
}

func TestEcho_ThroughRelay(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	sizes := []int{64 * 1024, 1 << 20, 64 << 20}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			ch := fmt.Sprintf("ch-%d-%d", size, time.Now().UnixNano())
			srv.RegisterChannel(ch, 30*time.Second)

			devLong := newEphKey(t)
			psnLong := newEphKey(t)

			go runEchoDaemon(t, baseURL, ch, psnLong, devLong.PublicKey())

			// app 端稍等 daemon attach 完成（attachSide 支持任意先后序，这里图省事）。
			time.Sleep(150 * time.Millisecond)
			ep := dialTunnel(t, baseURL, ch, devLong, psnLong.PublicKey())
			defer ep.Close()

			want := make([]byte, size)
			if _, err := rand.Read(want); err != nil {
				t.Fatal(err)
			}

			writeDone := make(chan error, 1)
			go func() {
				n, err := ep.Write(want)
				if err == nil && n != len(want) {
					err = fmt.Errorf("short write: %d/%d", n, len(want))
				}
				writeDone <- err
			}()

			got := make([]byte, size)
			if _, err := io.ReadFull(ep, got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if err := <-writeDone; err != nil {
				t.Fatalf("write: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("echo mismatch: got %d bytes, first diff at %d", len(got), firstDiff(got, want))
			}
		})
	}
}

// TestChannelExpiry：TTL 过后两端 attach 被拒（负向对照）。
func TestChannelExpiry(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	ch := "ch-expired"
	srv.RegisterChannel(ch, 50*time.Millisecond)
	time.Sleep(120 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, fmt.Sprintf("%s/tunnel/%s", baseURL, ch), nil)
	if err == nil {
		t.Fatal("expired channel accepted")
	}
	if resp != nil && resp.StatusCode != 404 {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func newEphKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}
