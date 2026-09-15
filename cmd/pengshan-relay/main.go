// pengshan-relay 是蓬山 relay 服务端入口：无秘密的对接交换机（见 internal/relayctl）。
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
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := relayctl.NewServer(logger)

	log.Printf("pengshan-relay listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
