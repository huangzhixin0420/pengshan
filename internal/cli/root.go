// Package cli 实现 pengshan 命令行。
package cli

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/netutil"
)

// NewRoot 组装根命令。
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "pengshan",
		Short: "蓬山：青鸟的配套守护进程（设备配对 / relay 反连 / E2E 隧道 / serve 桥接）",
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newPairCmd())
	root.AddCommand(newDevicesCmd())
	root.AddCommand(newUnpairCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newAutostartCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newUpdateCmd())
	return root
}

// lanIPv4s 见 netutil.LANAddrs（此处保留别名减少 diff 面）。
func lanIPv4s() []string { return netutil.LANAddrs() }

// pickClaimPort 从 preferred 开始找空闲 TCP 端口。
func pickClaimPort(preferred int) (int, error) {
	for p := preferred; p < preferred+20; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			_ = ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in [%d, %d)", preferred, preferred+20)
}
