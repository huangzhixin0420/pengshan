// pengshan-relay 是蓬山 relay 服务端入口：无秘密的对接交换机（见 internal/relayctl）。
// 公网部署必须设 token（-token 或 PENGSHAN_RELAY_TOKEN），/control 校验 Bearer。
package main

import (
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/huangzhixin0420/pengshan/internal/relayctl"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9400", "listen address")
	tokenFlag := flag.String("token", "", "control 鉴权 Bearer token（默认读 PENGSHAN_RELAY_TOKEN）")
	flag.Parse()

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("PENGSHAN_RELAY_TOKEN")
	}
	if token == "" {
		log.Println("警告：未设置 -token / PENGSHAN_RELAY_TOKEN，/control 无鉴权——仅限本机回环测试")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := relayctl.NewServer(logger, token)

	log.Printf("pengshan-relay listening on %s (auth=%v)", *addr, token != "")
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
