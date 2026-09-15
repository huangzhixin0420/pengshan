// Package relayclient 是 daemon 侧连 relay 的客户端。
// control 连接是事件驱动的：读循环把 relay 的帧分发给回调，
// 写路径带锁（pong / attach-deny 并发安全）。
package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
)

// controlFrame 与 relayctl 对齐（relay 可见信令帧）。
type controlFrame struct {
	V       int             `json:"v"`
	T       string          `json:"t"`
	Payload json.RawMessage `json:"p,omitempty"`
}

// Control 是 daemon → relay 的 control socket。
type Control struct {
	conn *websocket.Conn
	url  string // relay base（ws://…/ wss://…）

	// 事件回调（读循环内调用；不得阻塞，各启 goroutine）。
	onAttachRequest func(attachID string)
	onClose         func()

	writeMu sync.Mutex
}

// DialControl 连 relay /control 并完成注册（M3 无鉴权，register 帧仅声明角色；
// bootstrap/refresh token 在 M4 分发阶段加入）。
func DialControl(ctx context.Context, baseURL, daemonID string) (*Control, error) {
	conn, _, err := websocket.Dial(ctx, baseURL+"/control", nil)
	if err != nil {
		return nil, fmt.Errorf("dial control: %w", err)
	}
	conn.SetReadLimit(e2e.ReadLimit)
	c := &Control{conn: conn, url: baseURL}
	reg, _ := json.Marshal(controlFrame{
		V: 1, T: "register",
		Payload: json.RawMessage(`{"role":"daemon","id":` + jsonString(daemonID) + `}`),
	})
	if err := c.write(reg); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("register: %w", err)
	}
	go c.readLoop()
	return c, nil
}

// OnAttachRequest 注册 attach-request 回调（relay 通知有 app 接入）。
func (c *Control) OnAttachRequest(f func(attachID string)) { c.onAttachRequest = f }

// OnClose 注册 control 断开回调。
func (c *Control) OnClose(f func()) { c.onClose = f }

// readLoop 读循环：分发帧，断开时触发 onClose。
func (c *Control) readLoop() {
	defer func() {
		if c.onClose != nil {
			c.onClose()
		}
	}()
	for {
		var f controlFrame
		if err := c.readJSON(&f); err != nil {
			return
		}
		switch f.T {
		case "ping":
			b, _ := json.Marshal(controlFrame{V: 1, T: "pong"})
			_ = c.write(b)
		case "attach-request":
			var p struct {
				AttachID string `json:"attach_id"`
			}
			if err := json.Unmarshal(f.Payload, &p); err == nil && p.AttachID != "" && c.onAttachRequest != nil {
				c.onAttachRequest(p.AttachID)
			}
		}
	}
}

// Topology 是 daemon 周期上报的快照（与 relayctl 的 topologyPayload 同构 JSON）。
type Topology struct {
	LANEndpoints []string `json:"lan_endpoints,omitempty"`
	ServeOK      bool     `json:"serve_ok"`
	ServeAddr    string   `json:"serve_addr,omitempty"`
	Version      string   `json:"version,omitempty"`
	Devices      int      `json:"devices,omitempty"`
}

// SendTopology 上报网络/服务快照（relay 存最新一份供 app 择优）。
func (c *Control) SendTopology(t Topology) error {
	payload, _ := json.Marshal(t)
	b, _ := json.Marshal(controlFrame{
		V:       1,
		T:       "topology",
		Payload: payload,
	})
	return c.write(b)
}

// AttachDeny 拒绝一次 attach（设备吊销/内部错误），relay 会关闭 app 侧。
func (c *Control) AttachDeny(attachID, reason string) error {
	b, _ := json.Marshal(controlFrame{
		V: 1, T: "attach-deny",
		Payload: json.RawMessage(fmt.Sprintf(`{"attach_id":%s,"reason":%s}`, jsonString(attachID), jsonString(reason))),
	})
	return c.write(b)
}

// Ping 主动探活（5s 超时等 pong）。
func (c *Control) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.writeMu.Lock()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"ping"}`)); err != nil {
		c.writeMu.Unlock()
		return err
	}
	c.writeMu.Unlock()
	for {
		var f controlFrame
		if err := c.readJSON(&f); err != nil {
			return err
		}
		if f.T == "pong" {
			return nil
		}
	}
}

// readJSON / write 的并发模型：读循环独占读；写锁保护并发写（pong/deny/Ping）。
// 注意 Ping 与 readLoop 并发读 conn 是冲突的——Ping 仅用于测试连通性，
// 生产心跳用 WS 层 ping（见下方 PingWS）。
func (c *Control) readJSON(f *controlFrame) error {
	_, data, err := c.conn.Read(context.Background())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, f)
}

func (c *Control) write(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(context.Background(), websocket.MessageText, b)
}

// PingWS 发 WS 协议层 ping（单写锁，不等 pong；对端协议栈自动回）。
// 生产心跳用这个，不与读循环抢读。
func (c *Control) PingWS() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Ping(context.Background())
}

// Close 关闭 control。
func (c *Control) Close() error {
	if c.conn != nil {
		return c.conn.CloseNow()
	}
	return nil
}

// ErrNotConnected 占位（M4 用）。
var ErrNotConnected = errors.New("relayclient: control not connected")

// Attach 作为 daemon 侧挂到 /attach/{id}，返回已升级 WS。
func Attach(ctx context.Context, baseURL, attachID string) (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("%s/attach/%s", baseURL, attachID), nil)
	if err != nil {
		return nil, fmt.Errorf("dial attach: %w", err)
	}
	conn.SetReadLimit(e2e.ReadLimit)
	return conn, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
