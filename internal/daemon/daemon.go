// Package daemon 组装蓬山常驻进程：identity + storage + relay control + 桥接。
// 生命周期（M3）：前台运行（pengshan run）；start/stop/launchd 在 M4。
package daemon

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/bridge"
	"github.com/huangzhixin0420/pengshan/internal/config"
	"github.com/huangzhixin0420/pengshan/internal/e2e"
	"github.com/huangzhixin0420/pengshan/internal/identity"
	"github.com/huangzhixin0420/pengshan/internal/netutil"
	"github.com/huangzhixin0420/pengshan/internal/relayclient"
	"github.com/huangzhixin0420/pengshan/internal/storage"
	"github.com/huangzhixin0420/pengshan/internal/tunnel"
	"github.com/huangzhixin0420/pengshan/internal/version"
)

// Config 是 daemon 运行参数。
type Config struct {
	RelayURL   string // ws(s)://relay 地址
	RelayToken string // /control Bearer（公网 relay 鉴权；本机回环可空）
	ServeAddr  string // 127.0.0.1:9121 等
	DaemonID   string // control 注册 ID（展示用，如 "psn"）
}

// Run 阻塞运行：连 control → 等 attach 事件 → 每条 attach 起一个
// 握手 + 桥接 goroutine。ctx 取消即退。
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	priv, err := identity.NewStore().LoadOrCreate()
	if err != nil {
		return err
	}
	store, err := storage.Open(dbPath())
	if err != nil {
		return err
	}
	defer store.Close()

	d := &daemon{
		cfg:    cfg,
		priv:   priv,
		store:  store,
		logger: logger,
	}

	// 重连循环：断线指数退避 3→60s（DEV-PLAN §4.3）。
	backoff := 3 * time.Second
	for {
		err := d.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		logger.Warn("control disconnected, reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		} else {
			backoff = 60 * time.Second
		}
	}
}

type daemon struct {
	cfg    Config
	priv   *ecdh.PrivateKey
	store  *storage.Store
	logger *slog.Logger
}

func dbPath() string {
	return config.Dir() + "/pengshan.db"
}

func (d *daemon) runOnce(ctx context.Context) error {
	ctrl, err := relayclient.DialControl(ctx, d.cfg.RelayURL, d.cfg.DaemonID, d.cfg.RelayToken)
	if err != nil {
		return err
	}
	defer ctrl.Close()

	closed := make(chan struct{})
	ctrl.OnClose(func() { close(closed) })
	ctrl.OnAttachRequest(func(attachID string) {
		go d.handleAttach(ctx, ctrl, attachID)
	})

	// WS 层心跳 25s + topology 上报 20s（同 ticker 错峰即可，不必精细）。
	hb := time.NewTicker(25 * time.Second)
	defer hb.Stop()
	topo := time.NewTicker(20 * time.Second)
	defer topo.Stop()
	// 连接建立立即上报一帧（app 立刻可查）。
	d.reportTopology(ctrl)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-closed:
			return fmt.Errorf("control closed by relay")
		case <-hb.C:
			if err := ctrl.PingWS(); err != nil {
				return err
			}
		case <-topo.C:
			d.reportTopology(ctrl)
		}
	}
}

// reportTopology 采集本机网络/serve 快照上报 relay。
func (d *daemon) reportTopology(ctrl *relayclient.Control) {
	devs, _ := d.store.ListDevices()
	serveOK := bridge.Probe(d.cfg.ServeAddr) == nil
	_ = ctrl.SendTopology(relayclient.Topology{
		LANEndpoints: netutil.LANAddrs(),
		ServeOK:      serveOK,
		ServeAddr:    d.cfg.ServeAddr,
		Version:      version.Version,
		Devices:      len(devs),
	})
}

// handleAttach 处理一次 app 接入：回连 → 读 hello → 验设备 → 握手 → 桥接。
func (d *daemon) handleAttach(ctx context.Context, ctrl *relayclient.Control, attachID string) {
	log := d.logger.With("attach_id", attachID[:8])
	log.Info("attach requested")

	conn, err := relayclient.Attach(ctx, d.cfg.RelayURL, attachID)
	if err != nil {
		log.Error("attach dial", "error", err)
		_ = ctrl.AttachDeny(attachID, "attach dial failed")
		return
	}
	// 读 hello（10s 限时）。
	conn.SetReadLimit(1 << 20)
	helloCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, hello, err := conn.Read(helloCtx)
	cancel()
	if err != nil {
		log.Error("read hello", "error", err)
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "hello timeout")
		return
	}

	// 验设备：hello 里的 device_id 必须已配对未吊销。
	var env struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(hello, &env); err != nil || env.DeviceID == "" {
		log.Warn("bad hello envelope")
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "bad hello")
		return
	}
	pubRaw, err := d.store.GetDevicePubKey(env.DeviceID)
	if err != nil {
		log.Warn("device rejected", "device_id", env.DeviceID, "error", err)
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "device not paired")
		return
	}
	devPub, err := ecdh.X25519().NewPublicKey(pubRaw)
	if err != nil {
		log.Error("stored pubkey corrupt", "error", err)
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "device key corrupt")
		return
	}

	// e2e 握手（responder 视角）。
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "ephemeral failed")
		return
	}
	responder := e2e.NewResponder(d.priv, ephemeral, devPub)
	respond, keys, _, err := responder.Respond(hello)
	if err != nil {
		log.Warn("handshake rejected", "error", err)
		_ = conn.CloseNow()
		_ = ctrl.AttachDeny(attachID, "handshake failed")
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, respond); err != nil {
		_ = conn.CloseNow()
		return
	}

	_ = d.store.TouchDevice(env.DeviceID, time.Now())
	log.Info("tunnel established", "device_id", env.DeviceID)
	tun := tunnel.New(conn, keys)
	defer tun.Close()

	if err := bridge.Run(tun, d.cfg.ServeAddr, log); err != nil {
		log.Info("bridge closed", "error", err)
		return
	}
}
