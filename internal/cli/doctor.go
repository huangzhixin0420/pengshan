package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/autostart"
	"github.com/huangzhixin0420/pengshan/internal/bridge"
	"github.com/huangzhixin0420/pengshan/internal/config"
	"github.com/huangzhixin0420/pengshan/internal/version"
)

func newAutostartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autostart",
		Short: "开机自启管理（登录即起、stop 不自拉起）",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "on",
		Short: "启用自启",
		RunE: func(*cobra.Command, []string) error {
			if err := autostart.Install(); err != nil {
				return err
			}
			fmt.Println("自启已启用（登录时自动运行 pengshan run）。")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "off",
		Short: "关闭自启",
		RunE: func(*cobra.Command, []string) error {
			if err := autostart.Uninstall(); err != nil {
				return err
			}
			fmt.Println("自启已关闭。")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "查看自启状态",
		Run: func(*cobra.Command, []string) {
			fmt.Printf("自启配置：%v\n进程运行：%v\n", autostart.Installed(), autostart.Running())
		},
	})
	return cmd
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "装后自检：配置/身份/serve/relay/自启逐项体检",
		RunE: func(*cobra.Command, []string) error {
			var fails int
			check := func(ok bool, name, detail string) {
				mark := "✓"
				if !ok {
					mark = "✗"
					fails++
				}
				fmt.Printf("%s %-12s %s\n", mark, name, detail)
			}

			// 1) 版本
			check(version.Version != "dev", "version", version.Version+"（dev 构建提示：release 构建才有更新能力）")

			// 2) 数据目录权限
			dir := config.Dir()
			if perm := dirPerm(dir); perm != "" {
				check(perm == "drwx------", "dir", dir+" ("+perm+")")
			} else {
				check(false, "dir", dir+" 不存在（pengshan pair 会创建）")
			}

			// 3) identity
			idPath := dir + "/identity.key"
			if info, err := statFile(idPath); err == nil {
				check(info == "-rw-------", "identity", idPath)
			} else {
				check(false, "identity", "未生成（运行 pengshan pair 自动生成）")
			}

			// 4) 配置
			cfg, err := config.Load()
			check(err == nil, "config", config.Dir()+"/config.json")
			if err == nil {
				check(cfg.ServeAddr != "", "serve_addr", orDash(cfg.ServeAddr, "未配置：pengshan config set serve_addr 127.0.0.1:9121"))
				check(len(cfg.RelayURLs) > 0, "relay_urls", orDash(strings.Join(cfg.RelayURLs, ", "), "未配置：pengshan config set relay_urls '[\"ws://…\"]'"))
			}

			// 5) serve 可达（probe + healthz）
			if err == nil && cfg.ServeAddr != "" {
				healthErr := probeHealth(cfg.ServeAddr)
				check(healthErr == nil, "serve", cfg.ServeAddr+suffixErr(healthErr))
			} else {
				check(false, "serve", "跳过（无 serve_addr）")
			}

			// 6) relay 可达
			if err == nil && len(cfg.RelayURLs) > 0 {
				relayErr := probeRelay(cfg.RelayURLs[0])
				check(relayErr == nil, "relay", cfg.RelayURLs[0]+suffixErr(relayErr))
			} else {
				check(false, "relay", "跳过（无 relay_urls）")
			}

			// 7) 自启
			check(autostart.Installed(), "autostart", map[bool]string{true: "已启用", false: "未启用（pengshan autostart on）"}[autostart.Installed()])

			if fails > 0 {
				fmt.Printf("\n%d 项待处理。\n", fails)
				return fmt.Errorf("doctor: %d failed", fails)
			}
			fmt.Println("\n全部通过。")
			return nil
		},
	}
}

func suffixErr(err error) string {
	if err == nil {
		return ""
	}
	return "（" + err.Error() + "）"
}

func orDash(s, dash string) string {
	if s == "" {
		return dash
	}
	return s
}

// dirPerm 返回目录权限串（"drwx------"）；不存在返回 ""。
func dirPerm(path string) string {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return ""
	}
	return fi.Mode().String()
}

// statFile 返回文件权限串（"-rw-------"）。
func statFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fi.Mode().String(), nil
}

func probeHealth(addr string) error {
	if err := bridge.Probe(addr); err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	return nil
}

// probeRelay 把 ws(s):// 换成 http(s):// 打 /healthz。
func probeRelay(rawURL string) error {
	url := strings.Replace(rawURL, "ws://", "http://", 1)
	url = strings.Replace(url, "wss://", "https://", 1)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	return nil
}
