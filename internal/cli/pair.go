package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/config"
	"github.com/huangzhixin0420/pengshan/internal/identity"
	"github.com/huangzhixin0420/pengshan/internal/pairing"
	"github.com/huangzhixin0420/pengshan/internal/storage"
	"github.com/huangzhixin0420/pengshan/internal/version"
)

const defaultClaimPort = 9517

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本信息",
		Run: func(*cobra.Command, []string) {
			fmt.Printf("pengshan %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		},
	}
}

// newPairCmd 扫码配对：建会话 → 显示 QR/码/URL → 临时 HTTP 等 claim → 注册设备。
func newPairCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "生成配对二维码，等待青鸟 App 扫码 claim",
		RunE: func(cmd *cobra.Command, _ []string) error {
			priv, err := identity.NewStore().LoadOrCreate()
			if err != nil {
				return err
			}
			store, err := storage.Open(dbPath())
			if err != nil {
				return err
			}
			defer store.Close()

			mgr := pairing.NewManager(store, "psn", priv.PublicKey())

			// LAN 地址探测 → claim URL 列表。
			port, err := pickClaimPort(defaultClaimPort)
			if err != nil {
				return err
			}
			ips := lanIPv4s()
			var claimURLs []string
			for _, ip := range ips {
				claimURLs = append(claimURLs, fmt.Sprintf("http://%s:%d/pairing/claim", ip, port))
			}
			// 回环兜底（模拟器/本机调试）。
			claimURLs = append(claimURLs, fmt.Sprintf("http://127.0.0.1:%d/pairing/claim", port))

			sess, qr, err := mgr.CreateSession(claimURLs, note)
			if err != nil {
				return err
			}

			asciiQR, err := pairing.RenderQR(qr)
			if err != nil {
				return err
			}

			fmt.Printf("配对码：%s（10 分钟内有效）\n", sess.Code)
			fmt.Printf("会话 ID：%s\n\n", sess.SessionID)
			fmt.Println(asciiQR)
			fmt.Println("扫码或手动输入配对码。claim 地址：")
			for _, u := range claimURLs {
				fmt.Printf("  %s\n", u)
			}
			fmt.Println("等待 claim…（Ctrl-C 取消）")

			// 临时 claim server。
			claimed := make(chan *storage.Device, 1)
			claimErr := make(chan error, 1)
			mux := http.NewServeMux()
			mux.HandleFunc("POST /pairing/claim", func(w http.ResponseWriter, r *http.Request) {
				var req pairing.ClaimRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					writeJSON(w, http.StatusBadRequest, pairing.ClaimResponse{OK: false, Error: "bad request"})
					return
				}
				dev, err := mgr.Claim(req)
				if err != nil {
					status := http.StatusConflict
					if errors.Is(err, storage.ErrPairingNotFound) {
						status = http.StatusNotFound
					}
					writeJSON(w, status, pairing.ClaimResponse{OK: false, Error: err.Error()})
					return
				}
				writeJSON(w, http.StatusOK, pairing.ClaimResponse{
					OK: true, DeviceID: dev.DeviceID, PsnID: "psn",
				})
				claimed <- dev
			})
			srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
			go func() {
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					claimErr <- err
				}
			}()

			// 等 claim / Ctrl-C / 超时。
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			defer signal.Stop(sig)
			timeout := time.After(pairing.CodeTTL)

			select {
			case dev := <-claimed:
				fmt.Printf("\n✓ 配对成功：%s（%s / %s）\n", dev.DeviceID, dev.Label, dev.Platform)
				fmt.Println("该设备现在可经 relay 隧道连接本机。")
			case err := <-claimErr:
				return err
			case <-sig:
				fmt.Println("\n已取消。")
			case <-timeout:
				fmt.Println("\n配对码过期。")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return srv.Shutdown(ctx)
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "配对备注（识别用途）")
	return cmd
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// dbPath 是运行时 SQLite 路径。
func dbPath() string {
	return config.Dir() + "/pengshan.db"
}
