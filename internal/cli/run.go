package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/config"
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
			// 解析参数：flag 优先，config 兜底。
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if relayURL == "" && len(cfg.RelayURLs) > 0 {
				relayURL = cfg.RelayURLs[0]
			}
			if serveAddr == "" {
				serveAddr = cfg.ServeAddr
			}
			if relayURL == "" || serveAddr == "" {
				return fmt.Errorf("relay/serve 未配置：\n" +
					"  pengshan config set serve_addr 127.0.0.1:9121\n" +
					"  pengshan config set relay_urls '[\"ws://127.0.0.1:9400\"]'\n" +
					"或用 --relay/--serve 临时指定")
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
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay 地址（覆盖 config relay_urls[0]）")
	cmd.Flags().StringVar(&serveAddr, "serve", "", "本地 hermes serve 地址（覆盖 config serve_addr）")
	return cmd
}
