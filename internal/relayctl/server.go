// Package relayctl 是蓬山 relay 服务端：无秘密的对接交换机。
// 路由（M1）：
//
//	GET /control       daemon 注册 control socket（心跳/信令；M2 起带 bootstrap 鉴权）
//	GET /tunnel/{ch}   app 挂隧道一端
//	GET /attach/{ch}   daemon 挂隧道另一端；两端齐 → 双向 message pump
//	GET /healthz       存活探针
//
// relay 不落盘、不持密钥：channels 是纯内存映射，channel_id（128bit 随机）
// 由 daemon 经 control 开隧道时指定（M2 接线；M1 由 Registry.RegisterChannel 直注，
// 模拟"配对通道下发 ch"）。ch 有 TTL，两端 attach 即消费、任一端断开即清。
package relayctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
)

// ChannelTTL 是 channel 从创建到两端 attach 完成的窗口。
const ChannelTTL = 60 * time.Second

var (
	ErrChannelNotFound = errors.New("relay: channel not found")
	ErrChannelExpired  = errors.New("relay: channel expired")
	ErrChannelBusy     = errors.New("relay: channel already attached")
)

// controlFrame 是 control socket 的 JSON 帧（relay 可见、不加密）。
type controlFrame struct {
	V       int             `json:"v"`
	T       string          `json:"t"` // register/ping/pong/open/close/attach-request
	Payload json.RawMessage `json:"p,omitempty"`
}

type channel struct {
	id     string
	exp    time.Time
	mu     sync.Mutex
	app    *websocket.Conn
	daemon *websocket.Conn
	// attachCh 在两端集齐时关闭，通知等待端放行。
	attachCh chan struct{}
	once     sync.Once
	done     chan struct{}
	// pumpOnce 保证对接泵全 relay 只启动一次（两端 attachSide 都会走到，
	// 先到端在 select 唤醒后、后到端在 close(attachCh) 后，竞争只放行一个）。
	pumpOnce sync.Once
}

func (c *channel) complete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.app != nil && c.daemon != nil
}

// completeLocked 是持锁版（attachSide 已持 ch.mu 时调用，避免自死锁）。
func (c *channel) completeLocked() bool {
	return c.app != nil && c.daemon != nil
}

// Server 是 relay 核心。
type Server struct {
	mu       sync.Mutex
	channels map[string]*channel
	// controls 仅用于生命周期联动：control 断开时清理其渠道（M2 起按 daemon 归属）。
	controls map[*websocket.Conn]struct{}
	logger   *slog.Logger
	now      func() time.Time // 可注入时钟（测试）
}

// NewServer 组装 relay。
func NewServer(logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		channels: make(map[string]*channel),
		controls: make(map[*websocket.Conn]struct{}),
		logger:   logger,
		now:      time.Now,
	}
}

// Handler 返回 HTTP 路由（httptest 与生产 main 共用）。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /control", s.handleControl)
	mux.HandleFunc("GET /tunnel/{ch}", s.handleTunnel)
	mux.HandleFunc("GET /attach/{ch}", s.handleAttach)
	return mux
}

// RegisterChannel 预登记 channel（M1 由测试/将来 pairing 通道调用；正常路径是 control open 帧）。
func (s *Server) RegisterChannel(id string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := &channel{
		id:       id,
		exp:      s.now().Add(ttl),
		attachCh: make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.channels[id] = ch
	time.AfterFunc(ttl, func() { s.evict(id, ErrChannelExpired) })
}

func (s *Server) evict(id string, cause error) {
	s.mu.Lock()
	ch, ok := s.channels[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.channels, id)
	s.mu.Unlock()
	ch.once.Do(func() { close(ch.done) })
	s.logger.Debug("channel evicted", "id", id, "cause", cause)
}

func (s *Server) getChannel(id string) (*channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return nil, ErrChannelNotFound
	}
	if s.now().After(ch.exp) {
		return nil, ErrChannelExpired
	}
	return ch, nil
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(e2e.ReadLimit)
	s.mu.Lock()
	s.controls[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.controls, conn)
		s.mu.Unlock()
		_ = conn.CloseNow()
		s.logger.Debug("control closed")
	}()
	s.logger.Debug("control registered")
	// M1 只应答 ping；open/close/attach-request 在 M2 接线。
	for {
		var f controlFrame
		if err := wsReadJSON(r.Context(), conn, &f); err != nil {
			return
		}
		switch f.T {
		case "ping":
			_ = wsWriteJSON(r.Context(), conn, controlFrame{V: 1, T: "pong"})
		}
	}
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	s.attachSide(w, r, true)
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	s.attachSide(w, r, false)
}

// attachSide 处理 /tunnel 与 /attach 的公共逻辑：登记一端、齐了对泵、错了清理。
func (s *Server) attachSide(w http.ResponseWriter, r *http.Request, isApp bool) {
	id := r.PathValue("ch")
	ch, err := s.getChannel(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(e2e.ReadLimit)

	ch.mu.Lock()
	slot := &ch.app
	if !isApp {
		slot = &ch.daemon
	}
	if *slot != nil {
		ch.mu.Unlock()
		_ = conn.CloseNow()
		s.evict(id, ErrChannelBusy)
		http.Error(w, ErrChannelBusy.Error(), http.StatusConflict)
		return
	}
	*slot = conn
	ready := ch.completeLocked()
	if ready {
		close(ch.attachCh)
	}
	ch.mu.Unlock()

	if !ready {
		// 等对端或超时/清理。
		select {
		case <-ch.attachCh:
		case <-ch.done:
			_ = conn.CloseNow()
			return
		case <-r.Context().Done():
			_ = conn.CloseNow()
			return
		}
	}

	s.logger.Debug("channel attached", "id", id, "app", isApp)
	s.pumpPair(id, ch)
}

// pumpPair 双向 message pump：保留 WS message 边界（隧道帧 = 单条文本消息）。
// 任一端 EOF/出错 → 两端关闭 + channel 清场。
// 两端 attachSide 都会调用：pumpOnce 放行竞争胜者（必是后到端，或先到端被唤醒后），
// 败者在 once.Do 外直接返回——handler 生命周期不绑连接（ws 已 hijack，pump 持有引用）。
func (s *Server) pumpPair(id string, ch *channel) {
	ch.pumpOnce.Do(func() {
		ch.mu.Lock()
		app, daemon := ch.app, ch.daemon
		ch.mu.Unlock()
		defer func() {
			_ = app.CloseNow()
			_ = daemon.CloseNow()
			s.evict(id, nil)
		}()

		ctx := context.Background()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); pumpConn(ctx, daemon, app) }() // app → daemon
		go func() { defer wg.Done(); pumpConn(ctx, app, daemon) }() // daemon → app
		wg.Wait()
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
