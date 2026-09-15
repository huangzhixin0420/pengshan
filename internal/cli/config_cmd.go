package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/config"
)

// newConfigCmd 读写 ~/.pengshan/config.json。
// set 的值先按 JSON 解析（数组/数字），失败退化为字符串——relay_urls 必须传 JSON 数组。
func newConfigCmd() *cobra.Command {
	cfg := &cobra.Command{
		Use:   "config",
		Short: "查看/修改 daemon 配置（serve_addr、relay_urls、log_level）",
	}
	cfg.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "打印配置文件路径",
		Run: func(*cobra.Command, []string) {
			fmt.Println(config.Dir() + "/config.json")
		},
	})
	cfg.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "读取配置项",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			var v any
			switch args[0] {
			case "serve_addr":
				v = c.ServeAddr
			case "relay_urls":
				v = c.RelayURLs
			case "log_level":
				v = c.LogLevel
			default:
				return fmt.Errorf("未知配置项 %q（可用：serve_addr / relay_urls / log_level）", args[0])
			}
			b, _ := json.Marshal(v)
			fmt.Println(string(b))
			return nil
		},
	})
	cfg.AddCommand(&cobra.Command{
		Use:   "set <key> <value>",
		Short: "写入配置项（relay_urls 传 JSON 数组，如 '[\"ws://1.2.3.4:9400\"]'）",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			key, raw := args[0], strings.TrimSpace(args[1])
			switch key {
			case "serve_addr":
				c.ServeAddr = raw
			case "relay_urls":
				var urls []string
				if err := json.Unmarshal([]byte(raw), &urls); err != nil {
					return fmt.Errorf("relay_urls 需 JSON 数组：%v", err)
				}
				c.RelayURLs = urls
			case "log_level":
				if raw != "debug" && raw != "info" && raw != "warn" && raw != "error" {
					return fmt.Errorf("log_level ∈ debug|info|warn|error")
				}
				c.LogLevel = raw
			default:
				return fmt.Errorf("未知配置项 %q", key)
			}
			if err := config.Save(c); err != nil {
				return err
			}
			fmt.Printf("已写入 %s = %s\n", key, raw)
			return nil
		},
	})
	return cfg
}
