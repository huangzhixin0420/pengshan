package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/daemon"
)

// newRunCmd 前台运行 daemon（调试/M4 前的常驻形态）。
func newRunCmd() *cobra.Command {
	var (
		relayURL  string
		serveAddr string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "前台运行蓬山 daemon（连 relay、等设备接入、桥接到本地 serve）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if relayURL == "" || serveAddr == "" {
				return fmt.Errorf("--relay 与 --serve 均必填（M4 起支持 config 持久化）")
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			logger.Info("pengshan daemon starting", "relay", relayURL, "serve", serveAddr)
			return daemon.Run(ctx, daemon.Config{
				RelayURL:  relayURL,
				ServeAddr: serveAddr,
				DaemonID:  "psn",
			}, logger)
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay 地址（ws://127.0.0.1:9400 或 wss://…）")
	cmd.Flags().StringVar(&serveAddr, "serve", "", "本地 hermes serve 地址（127.0.0.1:9121）")
	return cmd
}
