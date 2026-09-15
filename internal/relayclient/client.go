// Package relayclient 是 daemon 侧连 relay 的客户端。
// M1 只含 control 连接骨架（ping/pong）与 attach；bootstrap/refresh/open/拓扑在 M2/M3 接线。
package relayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
)

// Control 是 daemon → relay 的 control socket。
type Control struct {
	conn *websocket.Conn
	url  string
}

// controlFrame 与 relayctl 对齐（relay 可见信令帧）。
type controlFrame struct {
	V       int             `json:"v"`
	T       string          `json:"t"`
	Payload json.RawMessage `json:"p,omitempty"`
}

// DialControl 连 relay /control 并完成注册（M1 无鉴权，register 帧仅声明角色）。
func DialControl(ctx context.Context, baseURL, daemonID string) (*Control, error) {
	conn, _, err := websocket.Dial(ctx, baseURL+"/control", nil)
	if err != nil {
		return nil, fmt.Errorf("dial control: %w", err)
	}
	conn.SetReadLimit(e2e.ReadLimit)
	reg, _ := json.Marshal(controlFrame{
		V: 1, T: "register",
		Payload: json.RawMessage(`{"role":"daemon","id":` + jsonString(daemonID) + `}`),
	})
	if err := conn.Write(ctx, websocket.MessageText, reg); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("register: %w", err)
	}
	return &Control{conn: conn, url: baseURL}, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Ping 发 ping 并等 pong（5s 超时）。
func (c *Control) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, []byte(`{"v":1,"t":"ping"}`)); err != nil {
		return err
	}
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var f controlFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.T == "pong" {
			return nil
		}
	}
}

// Close 关闭 control。
func (c *Control) Close() error {
	if c.conn != nil {
		return c.conn.CloseNow()
	}
	return nil
}

// Attach 作为 daemon 侧挂到 /attach/{ch}，返回已升级 WS。
func Attach(ctx context.Context, baseURL, ch string) (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(ctx, fmt.Sprintf("%s/attach/%s", baseURL, ch), nil)
	if err != nil {
		return nil, fmt.Errorf("dial attach: %w", err)
	}
	conn.SetReadLimit(e2e.ReadLimit)
	return conn, nil
}
