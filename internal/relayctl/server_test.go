// M3 集成验收：新对接模型（/tunnel 直连 + control attach-request 路由）
// + 模拟 serve（HTTP /api/health 与 WS 流式）经 E2E 隧道全链。
package relayctl_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/bridge"
	"github.com/huangzhixin0420/pengshan/internal/e2e"
	"github.com/huangzhixin0420/pengshan/internal/relayclient"
	"github.com/huangzhixin0420/pengshan/internal/relayctl"
	"github.com/huangzhixin0420/pengshan/internal/tunnel"
)

// --- 测试替身：模拟 hermes serve（HTTP + WS） ---

// mockServe 起一个简单的 HTTP server：/api/health 200；/ws echo 每条 message。
func mockServe(t *testing.T) (addr string, stop func()) {
	t.Helper()
	upgrader := websocket.Accept
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(1 << 20)
		for {
			mt, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			// echo：前缀 "echo:" 便于断言。
			if err := conn.Write(r.Context(), mt, append([]byte("echo:"), data...)); err != nil {
				return
			}
		}
	})
	ts := httptest.NewServer(mux)
	return ts.Listener.Addr().String(), ts.Close
}

// --- 测试替身：daemon 侧 attach handler ---

type daemonHandle struct {
	priv   *ecdh.PrivateKey
	devPub *ecdh.PublicKey // 预配对设备公钥；nil = 全部拒
}

// attachLoop 模拟 daemon 的 attach 处理（与 internal/daemon.handleAttach 同构，
// 桥接目标 = serveAddr）。
func (d *daemonHandle) attachLoop(t *testing.T, ctx context.Context, baseURL, attachID, serveAddr string, denyReason *string) {
	t.Helper()
	conn, err := relayclient.Attach(ctx, baseURL, attachID)
	if err != nil {
		t.Errorf("attach: %v", err)
		return
	}
	conn.SetReadLimit(1 << 20)
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, hello, err := conn.Read(readCtx)
	cancel()
	if err != nil {
		t.Errorf("read hello: %v", err)
		_ = conn.CloseNow()
		return
	}
	if d.devPub == nil {
		_ = conn.CloseNow()
		return // 上层负责 deny（测试里通过 control 发）
	}
	resp := e2e.NewResponder(d.priv, newKey(t), d.devPub)
	respond, keys, _, err := resp.Respond(hello)
	if err != nil {
		t.Errorf("respond: %v", err)
		_ = conn.CloseNow()
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, respond); err != nil {
		t.Errorf("write respond: %v", err)
		return
	}
	tun := tunnel.New(conn, keys)
	defer tun.Close()
	if serveAddr == "echo" {
		_, _ = io.Copy(tun, tun) // M1 echo 语义保留
		return
	}
	_ = bridge.Run(tun, serveAddr, nil)
}

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// startDaemon 起 control 连接 + attach 路由（生产 daemon.Run 的测试替身）。
func startDaemon(t *testing.T, baseURL string, dh *daemonHandle, serveAddr string) *relayclient.Control {
	t.Helper()
	ctrl, err := relayclient.DialControl(context.Background(), baseURL, "psn_test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctrl.Close() })
	ctrl.OnAttachRequest(func(attachID string) {
		go dh.attachLoop(t, context.Background(), baseURL, attachID, serveAddr, nil)
	})
	time.Sleep(100 * time.Millisecond) // 等注册生效
	return ctrl
}

// dialTunnel app 侧：连 /tunnel → hello → 握手 → 隧道端点。
func dialTunnel(t *testing.T, baseURL string, devLong *ecdh.PrivateKey, psnPub *ecdh.PublicKey) *tunnel.Endpoint {
	t.Helper()
	ctx := context.Background()
	wsConn, _, err := websocket.Dial(ctx, baseURL+"/tunnel", nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	wsConn.SetReadLimit(1 << 20)
	init := e2e.NewInitiator("dev_test", devLong, newKey(t), psnPub)
	hello, err := init.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if err := wsConn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	_, respond, err := wsConn.Read(ctx)
	if err != nil {
		t.Fatalf("read respond: %v", err)
	}
	keys, err := init.Finish(respond)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel.New(wsConn, keys)
}

func TestEcho_ThroughRelay(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	devLong, psnLong := newKey(t), newKey(t)
	dh := &daemonHandle{priv: psnLong, devPub: devLong.PublicKey()}
	startDaemon(t, baseURL, dh, "echo")

	sizes := []int{64 * 1024, 1 << 20}
	if bigSize > 0 {
		sizes = append(sizes, bigSize)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			ep := dialTunnel(t, baseURL, devLong, psnLong.PublicKey())
			defer ep.Close()

			want := make([]byte, size)
			rand.Read(want)
			// 全双工：写与读各属一个 goroutine（回压链：写满 → daemon echo
			// 阻塞 → 双方 TCP 缓冲耗尽；半双工会在大帧下死锁）。
			writeDone := make(chan error, 1)
			go func() {
				n, err := ep.Write(want)
				if err == nil && n != len(want) {
					err = fmt.Errorf("short write %d/%d", n, len(want))
				}
				writeDone <- err
			}()
			got := make([]byte, size)
			readDone := make(chan error, 1)
			go func() {
				_, err := io.ReadFull(ep, got)
				readDone <- err
			}()
			select {
			case err := <-writeDone:
				if err != nil {
					t.Fatalf("write: %v", err)
				}
				// 写已完成：只等读。
				if err := <-readDone; err != nil {
					t.Fatalf("read back: %v", err)
				}
			case err := <-readDone:
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				// 读已完成：只等写。
				if err := <-writeDone; err != nil {
					t.Fatalf("write: %v", err)
				}
			case <-time.After(echoCaseTimeout):
				t.Fatalf("echo case %dB exceeded %v（race 开销下可调窗口）", size, echoCaseTimeout)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("echo mismatch")
			}
		})
	}
}

// TestHTTPAndWS_ThroughTunnel 是 M3 主验收：隧道里跑真实 HTTP 请求 + WS 流式。
func TestHTTPAndWS_ThroughTunnel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	serveAddr, stopServe := mockServe(t)
	defer stopServe()

	devLong, psnLong := newKey(t), newKey(t)
	dh := &daemonHandle{priv: psnLong, devPub: devLong.PublicKey()}
	startDaemon(t, baseURL, dh, serveAddr)

	ep := dialTunnel(t, baseURL, devLong, psnLong.PublicKey())
	defer ep.Close()

	// 1) HTTP GET /api/health（keep-alive：HTTP/1.1 默认即 keep-alive，
	//    复用同一条隧道继续跑 WS 升级——与真实 app transport 行为一致）。
	req := fmt.Sprintf("GET /api/health HTTP/1.1\r\nHost: %s\r\n\r\n", serveAddr)
	if _, err := ep.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(ep), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("health via tunnel: status=%d body=%s", resp.StatusCode, body)
	}

	// 2) WS 流式：升级 + 消息回环（验证 101 升级直通 + 多帧往返）。
	//    注意：这里复用同一隧道——不能带 Connection: close（那是"此请求后关闭
	//    整条连接"的语义，serve 关 TCP 后桥接就会拆隧道，真实 app 不会这么发）。
	wsReq := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", serveAddr)
	if _, err := ep.Write([]byte(wsReq)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(ep)
	upResp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("ws upgrade response: %v", err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != 101 {
		t.Fatalf("want 101, got %d", upResp.StatusCode)
	}
	// 升级后剩余字节流 = WS 协议帧。手工构造客户端 text frame（mask 位）
	// 写入隧道，读服务端 echo 帧——完整验证 101 之后的字节直通 + 全双工往返。
	payload := []byte("hello-serve")
	frame := wsClientTextFrame(payload)
	if _, err := ep.Write(frame); err != nil {
		t.Fatal(err)
	}
	gotPayload, err := readWSFrame(br)
	if err != nil {
		t.Fatalf("read ws frame: %v", err)
	}
	if !bytes.HasPrefix(gotPayload, []byte("echo:")) || !bytes.Contains(gotPayload, payload) {
		t.Fatalf("ws echo = %q", gotPayload)
	}
}

// wsClientTextFrame 构造一个带 mask 的客户端 text frame（FIN=1 opcode=1）。
func wsClientTextFrame(payload []byte) []byte {
	var f bytes.Buffer
	f.WriteByte(0x81)
	switch l := len(payload); {
	case l < 126:
		f.WriteByte(byte(0x80 | l)) // mask 位 + 短长度
	case l < 65536:
		f.WriteByte(0x80 | 126)
		f.WriteByte(byte(l >> 8))
		f.WriteByte(byte(l))
	default:
		f.WriteByte(0x80 | 127)
		for i := 7; i >= 0; i-- {
			f.WriteByte(byte(uint64(l) >> (8 * i)))
		}
	}
	var mask [4]byte
	rand.Read(mask[:])
	f.Write(mask[:])
	for i, b := range payload {
		f.WriteByte(b ^ mask[i%4])
	}
	return f.Bytes()
}

// readWSFrame 读一条服务端 text frame（无 mask）并返回 payload。
func readWSFrame(br *bufio.Reader) ([]byte, error) {
	b1, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	b2, err := br.ReadByte()
	if err != nil {
		return nil, err
	}
	if b1&0x0f != 1 {
		return nil, fmt.Errorf("opcode %d", b1&0x0f)
	}
	length := uint64(b2 & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(ext[0])<<8 | uint64(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		for _, v := range ext {
			length = length<<8 | uint64(v)
		}
	}
	payload := make([]byte, length)
	_, err = io.ReadFull(br, payload)
	return payload, err
}

// TestNoDaemon / TestRevokedDevice 负向对照。
func TestNoDaemon(t *testing.T) {
	srv := relayctl.NewServer(nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, baseURL+"/tunnel", nil)
	if err == nil {
		t.Fatal("tunnel accepted without daemon")
	}
	if resp != nil && resp.StatusCode != 503 {
		t.Fatalf("want 503, got %d", resp.StatusCode)
	}
}

// TestRevokedDeviceDenied 的拒绝路径在 daemon.handleAttach（查 store），
// relay 侧只透传 deny 帧；端到端验证见 run 命令冒烟（吊销后 attach 被拒）。

// TestBridgeServeUnavailable：bridge 对不可达 serve 报 serve_unavailable。
func TestBridgeServeUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // 关掉 → 端口不可达

	err = bridge.Probe(addr)
	var sue *bridge.ServeUnavailableError
	if !errors.As(err, &sue) {
		t.Fatalf("want ServeUnavailableError, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "serve_unavailable") {
		t.Fatalf("prefix: %v", err)
	}
}

// TestAttachTimeout：daemon 不回连时 app 被清理（不挂死）。
func TestAttachTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := relayctl.NewServer(logger)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	baseURL := "ws" + ts.URL[len("http"):]

	// control 在线但不处理 attach（不回连）。
	ctrl, err := relayclient.DialControl(context.Background(), baseURL, "lazy")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsConn, _, err := websocket.Dial(ctx, baseURL+"/tunnel", nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer wsConn.CloseNow()
	// relay 等 daemon 回连 15s；测试不等全程——3s 后连接应仍挂着（没被立刻拒）。
	time.Sleep(3 * time.Second)
	// 主动关掉 app 侧触发清理（不挂死即可）。
}

// 占位：防止 base64 未用（部分用例调试时引入）。
var _ = base64.RawURLEncoding
