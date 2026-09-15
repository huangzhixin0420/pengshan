package relayctl

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// wsReadJSON 读一条完整 message 并解析 JSON。
func wsReadJSON(ctx context.Context, c *websocket.Conn, v any) error {
	_, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// wsWriteJSON 序列化并写一条文本 message。
func wsWriteJSON(ctx context.Context, c *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, data)
}
