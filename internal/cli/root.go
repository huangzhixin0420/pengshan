// Package cli 实现 pengshan 命令行。
package cli

import (
	"fmt"
	"net"

	"github.com/spf13/cobra"
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

// lanIPv4s 返回本机所有非回环 IPv4（多网卡全列，app 择优）。
func lanIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

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
