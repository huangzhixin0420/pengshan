// Package relayctl 是蓬山 relay 服务端：无秘密的对接交换机。
//
// 数据面路由（M3 定稿）：
//
//	app  GET /tunnel        → relay 生成 attach_id，向唯一 control 发
//	                        attach-request {attach_id}，挂起等 daemon 回连
//	daemon GET /attach/{id} → relay 查 pending 表，两端齐 → 双向 message pump
//
// 为什么 relay 不读 app 的 hello 帧：单 daemon 阶段路由不需要 device_id
// （任何 attach-request 都发给唯一 control），hello 直通给 daemon，
// daemon 自己查 store 验设备并做 e2e 握手。多 daemon 路由（读 hello、
// device→control 路由表）留 Phase 1.5+，此处注释为界。
//
// relay 不落盘、不持密钥、不解密 payload。
package relayctl

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
)

// AttachTimeout 是 daemon 回连 /attach 的窗口。
const AttachTimeout = 15 * time.Second

var (
	ErrNoDaemon       = errors.New("relay: no daemon connected")
	ErrAttachNotFound = errors.New("relay: attach id not found")
	ErrAttachExpired  = errors.New("relay: attach id expired")
	ErrAttachConsumed = errors.New("relay: attach id already consumed")
)

// controlFrame 是 control socket 的 JSON 帧（relay 可见、不加密）。
// 方向：relay → daemon：ping / attach-request；
// 方向：daemon → relay：pong / attach-deny。
type controlFrame struct {
	V       int             `json:"v"`
	T       string          `json:"t"`
	Payload json.RawMessage `json:"p,omitempty"`
}

type attachRequestPayload struct {
	AttachID string `json:"attach_id"`
}

type attachDenyPayload struct {
	AttachID string `json:"attach_id"`
	Reason   string `json:"reason"`
}

// pendingAttach 是一次待对接的连接对。
// pumpOnce 保证对接泵只启动一次；doneOnce 独立保护清理信号——
// 两者必须分开：泵先启动后 once 即耗尽，若共用会让 close(done) 永远没机会执行。
type pendingAttach struct {
	id     string
	exp    time.Time
	mu     sync.Mutex
	app    *websocket.Conn
	daemon *websocket.Conn

	pumpOnce sync.Once
	done     chan struct{}
	doneOnce sync.Once
}

func (p *pendingAttach) completeLocked() bool { return p.app != nil && p.daemon != nil }

// Server 是 relay 核心。
type Server struct {
	mu       sync.Mutex
	attaches map[string]*pendingAttach
	controls map[*controlConn]struct{}
	logger   *slog.Logger
	now      func() time.Time
	// attachTimeout 是 daemon 回连窗口（测试可调小）。
	attachTimeout time.Duration
	// token 非空时 /control 要求 Authorization: Bearer <token>（公网部署必备）。
	token string
}

// topologyPayload 是 daemon 周期上报的网络/服务快照（relay 存最新一份，
// app 连接前查 /api/v1/topology 做 LAN 直连 vs relay 隧道择优）。
type topologyPayload struct {
	LANEndpoints []string `json:"lan_endpoints,omitempty"`
	ServeOK      bool     `json:"serve_ok"`
	ServeAddr    string   `json:"serve_addr,omitempty"`
	Version      string   `json:"version,omitempty"`
	Devices      int      `json:"devices,omitempty"`
}

type controlConn struct {
	ws  *websocket.Conn
	wmu sync.Mutex // control 写锁（pong 与 deny 并发写）

	mu         sync.Mutex // 保护快照字段
	topology   topologyPayload
	topologyAt time.Time
}

// NewServer 组装 relay。token 非空时 /control 校验 Bearer（PENGSHAN_RELAY_TOKEN）。
func NewServer(logger *slog.Logger, token string) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		attaches:      make(map[string]*pendingAttach),
		controls:      make(map[*controlConn]struct{}),
		logger:        logger,
		now:           time.Now,
		attachTimeout: AttachTimeout,
		token:         token,
	}
}

// SetAttachTimeoutForTest 缩小回连窗口（仅测试用）。
func (s *Server) SetAttachTimeoutForTest(d time.Duration) { s.attachTimeout = d }

// Handler 返回 HTTP 路由（httptest 与生产 main 共用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/v1/topology", s.handleTopology)
	mux.HandleFunc("GET /control", s.handleControl)
	mux.HandleFunc("GET /tunnel", s.handleTunnel)
	mux.HandleFunc("GET /attach/{id}", s.handleAttach)
	return mux
}

// --- control ---

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	if s.token != "" {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(e2e.ReadLimit)
	cc := &controlConn{ws: conn}
	s.mu.Lock()
	s.controls[cc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.controls, cc)
		s.mu.Unlock()
		_ = conn.CloseNow()
		s.logger.Debug("control closed")
	}()
	s.logger.Debug("control registered")
	for {
		var f controlFrame
		if err := wsReadJSON(r.Context(), conn, &f); err != nil {
			return
		}
		switch f.T {
		case "ping":
			_ = cc.writeJSON(controlFrame{V: 1, T: "pong"})
		case "attach-deny":
			var p attachDenyPayload
			if err := json.Unmarshal(f.Payload, &p); err == nil && p.AttachID != "" {
				s.failAttach(p.AttachID, errors.New(p.Reason))
			}
		case "topology":
			var p topologyPayload
			if err := json.Unmarshal(f.Payload, &p); err == nil {
				cc.mu.Lock()
				cc.topology = p
				cc.topologyAt = s.now()
				cc.mu.Unlock()
			}
		}
	}
}

// handleTopology 返回各 daemon 的最新快照（app 择优数据源）。
func (s *Server) handleTopology(w http.ResponseWriter, _ *http.Request) {
	type daemonSnapshot struct {
		Since    time.Duration   `json:"since_seconds"`
		Topology topologyPayload `json:"topology"`
	}
	out := make(map[string]daemonSnapshot)
	now := s.now()
	s.mu.Lock()
	for cc := range s.controls {
		cc.mu.Lock()
		if !cc.topologyAt.IsZero() {
			out[fmt.Sprintf("%p", cc)] = daemonSnapshot{
				Since:    now.Sub(cc.topologyAt).Round(time.Second),
				Topology: cc.topology,
			}
		}
		cc.mu.Unlock()
	}
	s.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"daemons": out})
}

// sendAttachRequest 把 app 的接入请求转给唯一 daemon control。
// 返回 attachID；当前无 daemon 或写失败返回错误。
func (s *Server) sendAttachRequest(attachID string) error {
	s.mu.Lock()
	var target *controlConn
	for cc := range s.controls {
		target = cc // 单 daemon：取第一个
		break
	}
	s.mu.Unlock()
	if target == nil {
		return ErrNoDaemon
	}
	err := target.writeJSON(controlFrame{
		V: 1, T: "attach-request",
		Payload: mustJSON(attachRequestPayload{AttachID: attachID}),
	})
	s.logger.Debug("attach-request sent", "id", attachID, "target", fmt.Sprintf("%p", target), "err", err)
	return err
}

// failAttach 由 daemon 拒绝（设备吊销/内部错误）时关闭 app 侧并清场。
func (s *Server) failAttach(id string, cause error) {
	s.mu.Lock()
	pa, ok := s.attaches[id]
	if ok {
		delete(s.attaches, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	pa.doneOnce.Do(func() { close(pa.done) })
	s.logger.Debug("attach denied", "id", id, "cause", cause)
}

func (s *Server) evictAttach(id string) {
	s.mu.Lock()
	pa, ok := s.attaches[id]
	if ok {
		delete(s.attaches, id)
	}
	s.mu.Unlock()
	if ok {
		pa.doneOnce.Do(func() { close(pa.done) })
	}
}

// hasDaemon 报告当前是否有 control 在线。
func (s *Server) hasDaemon() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.controls) > 0
}

// --- /tunnel（app 侧） ---

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.hasDaemon() {
		http.Error(w, ErrNoDaemon.Error(), http.StatusServiceUnavailable)
		return
	}
	var attachIDBytes [16]byte
	if _, err := rand.Read(attachIDBytes[:]); err != nil {
		http.Error(w, "rand failed", http.StatusInternalServerError)
		return
	}
	attachID := hexEncode(attachIDBytes[:])

	s.mu.Lock()
	pa := &pendingAttach{
		id:   attachID,
		exp:  s.now().Add(s.attachTimeout),
		done: make(chan struct{}),
	}
	s.attaches[attachID] = pa
	s.mu.Unlock()
	time.AfterFunc(s.attachTimeout, func() { s.failAttachExpired(attachID) })

	// 2) Accept app
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.evictAttach(attachID)
		return
	}
	conn.SetReadLimit(e2e.ReadLimit)
	pa.mu.Lock()
	pa.app = conn
	ready := pa.completeLocked()
	pa.mu.Unlock()

	// 3) 通知 daemon（在 app 挂好之后，避免 daemon 瞬间回连时 app 还没登记）
	if err := s.sendAttachRequest(attachID); err != nil {
		_ = conn.CloseNow()
		s.evictAttach(attachID)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	if !ready {
		select {
		case <-pa.done:
			_ = conn.CloseNow()
			return
		case <-r.Context().Done():
			_ = conn.CloseNow()
			return
		}
	}
	s.logger.Debug("attach paired", "id", attachID)
	s.pumpAttach(attachID, pa)
}

// failAttachExpired 超时清理：直接按 id 清（存在即过期，不再二次判断）。
func (s *Server) failAttachExpired(id string) {
	s.failAttach(id, ErrAttachExpired)
}

func (s *Server) getAttach(id string) (*pendingAttach, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pa, ok := s.attaches[id]
	if !ok {
		return nil, ErrAttachNotFound
	}
	if s.now().After(pa.exp) {
		return nil, ErrAttachExpired
	}
	return pa, nil
}

// --- /attach（daemon 侧） ---

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pa, err := s.getAttach(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(e2e.ReadLimit)

	pa.mu.Lock()
	if pa.daemon != nil {
		pa.mu.Unlock()
		_ = conn.CloseNow()
		http.Error(w, ErrAttachConsumed.Error(), http.StatusConflict)
		return
	}
	pa.daemon = conn
	ready := pa.completeLocked()
	pa.mu.Unlock()

	if !ready {
		select {
		case <-pa.done:
			_ = conn.CloseNow()
			return
		case <-r.Context().Done():
			_ = conn.CloseNow()
			return
		}
	}
	s.logger.Debug("attach paired", "id", id)
	s.pumpAttach(id, pa)
}

// --- 对接泵 ---

// pumpAttach 双向 message pump（once 保证只启动一次）。
// 退出语义：任一方向 EOF/出错 → 立即两端 CloseNow，另一方向的阻塞 Read
// 会因连接关闭而返回——不能等两个方向都退（app 静默时它对端方向的读
// 永远阻塞，deny/桥接断开会被架空）。
func (s *Server) pumpAttach(id string, pa *pendingAttach) {
	pa.pumpOnce.Do(func() {
		pa.mu.Lock()
		app, daemon := pa.app, pa.daemon
		pa.mu.Unlock()
		defer func() {
			_ = app.CloseNow()
			_ = daemon.CloseNow()
			s.evictAttach(id)
		}()
		ctx := context.Background()
		done := make(chan struct{}, 2)
		go func() { pumpConn(ctx, daemon, app); done <- struct{}{} }()
		go func() { pumpConn(ctx, app, daemon); done <- struct{}{} }()
		<-done // 一个方向死 = 会话结束
	})
}

// pumpConn 逐 message 转发（读完整 message → 按原类型写出）。
func pumpConn(ctx context.Context, dst, src *websocket.Conn) {
	for {
		mt, r, err := src.Reader(ctx)
		if err != nil {
			return
		}
		w, err := dst.Writer(ctx, mt)
		if err != nil {
			return
		}
		_, copyErr := io.Copy(w, r)
		closeErr := w.Close()
		if copyErr != nil || closeErr != nil {
			return
		}
	}
}

// --- 工具 ---

func (c *controlConn) writeJSON(v controlFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return wsWriteJSON(context.Background(), c.ws, v)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0xf]
	}
	return string(out)
}
