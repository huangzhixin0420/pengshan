package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/storage"
)

func newDevicesCmd() *cobra.Command {
	dev := &cobra.Command{
		Use:   "devices",
		Short: "已配对设备管理",
	}
	dev.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "列出未吊销设备",
		RunE: func(*cobra.Command, []string) error {
			store, err := storage.Open(dbPath())
			if err != nil {
				return err
			}
			defer store.Close()
			devs, err := store.ListDevices()
			if err != nil {
				return err
			}
			if len(devs) == 0 {
				fmt.Println("暂无已配对设备。运行 pengshan pair 开始配对。")
				return nil
			}
			fmt.Printf("%-22s %-16s %-10s %s\n", "DEVICE ID", "LABEL", "PLATFORM", "PAIRED AT")
			for _, d := range devs {
				fmt.Printf("%-22s %-16s %-10s %s\n", d.DeviceID, d.Label, d.Platform, d.PairedAt.Local().Format("2006-01-02 15:04"))
			}
			return nil
		},
	})
	dev.AddCommand(&cobra.Command{
		Use:   "revoke <device_id>",
		Short: "吊销设备（立即失效，需重新配对）",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := storage.Open(dbPath())
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.RevokeDevice(args[0], time.Now()); err != nil {
				return err
			}
			_ = store.AppendAudit(time.Now(), "local_cli", "", "device.revoked", map[string]string{"device_id": args[0]})
			fmt.Printf("已吊销 %s\n", args[0])
			return nil
		},
	})
	return dev
}

func newUnpairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpair",
		Short: "吊销全部设备并重置配对（需确认）",
		RunE: func(*cobra.Command, []string) error {
			fmt.Print("确认吊销全部设备？[y/N] ")
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "y" {
				fmt.Println("已取消。")
				return nil
			}
			store, err := storage.Open(dbPath())
			if err != nil {
				return err
			}
			defer store.Close()
			n, err := store.RevokeAllDevices(time.Now())
			if err != nil {
				return err
			}
			_ = store.AppendAudit(time.Now(), "local_cli", "", "devices.revoked_all", map[string]int64{"count": n})
			fmt.Printf("已吊销 %d 台设备。\n", n)
			return nil
		},
	}
}
