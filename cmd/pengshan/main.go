// pengshan 是蓬山 daemon 的 CLI 入口。
package main

import (
	"os"

	"github.com/huangzhixin0420/pengshan/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
