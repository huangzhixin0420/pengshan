// pengshan 是蓬山 daemon 的 CLI 入口。
// M1 只落地 version 命令骨架；pair/devices/start/stop 等在 M2 逐条加入。
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:   "pengshan",
		Short: "蓬山：青鸟的配套守护进程（设备配对 / relay 反连 / E2E 隧道 / serve 桥接）",
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "打印版本信息",
		Run: func(*cobra.Command, []string) {
			fmt.Printf("pengshan %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		},
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
