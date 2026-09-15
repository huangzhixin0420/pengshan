// Package autostart 安装/卸载 daemon 的开机自启。
// 语义（DEV-PLAN §6）：登录即起、崩溃不自动拉起、用户 stop 后保持停
// （launchd RunAtLoad=true + KeepAlive=false；systemd WantedBy + Restart=no）。
package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/huangzhixin0420/pengshan/internal/config"
)

// Label 是 launchd job 名 / systemd unit 名。
const Label = "cn.hzxprzp.pengshan"

// binPath 是安装目标：~/.pengshan/bin/pengshan。
func binPath() string {
	return filepath.Join(config.Dir(), "bin", "pengshan")
}

// Installed 报告当前是否已启用自启。
func Installed() bool {
	switch runtime.GOOS {
	case "darwin":
		_, err := os.Stat(filepath.Join(os.Getenv("HOME"), "Library/LaunchAgents", Label+".plist"))
		return err == nil
	case "linux":
		_, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config/systemd/user", Label+".service"))
		return err == nil
	}
	return false
}

// Running 报告 daemon 是否在跑（按当前进程表粗查，doctor 用）。
func Running() bool {
	out, err := exec.Command("pgrep", "-f", "pengshan run").Output()
	return err == nil && len(out) > 0
}

// Install 写入自启配置并加载。
func Install() error {
	bin := binPath()
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("二进制不存在：%s（先运行 install.sh 安装）", bin)
	}
	if err := os.MkdirAll(config.Dir()+"/logs", 0o700); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(bin)
	case "linux":
		return installSystemd(bin)
	default:
		return fmt.Errorf("不支持的平台 %s", runtime.GOOS)
	}
}

// Uninstall 卸载自启配置。
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		plist := filepath.Join(os.Getenv("HOME"), "Library/LaunchAgents", Label+".plist")
		_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)).Run()
		return os.Remove(plist)
	case "linux":
		unit := filepath.Join(os.Getenv("HOME"), ".config/systemd/user", Label+".service")
		_ = exec.Command("systemctl", "--user", "disable", "--now", Label).Run()
		return os.Remove(unit)
	}
	return nil
}

func installLaunchd(bin string) error {
	home := os.Getenv("HOME")
	agentsDir := filepath.Join(home, "Library/LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
  <key>StandardOutPath</key><string>%s/.pengshan/logs/daemon.log</string>
  <key>StandardErrorPath</key><string>%s/.pengshan/logs/daemon.log</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>%s</string>
  </dict>
</dict>
</plist>
`, Label, bin, home, home, home)
	plistPath := filepath.Join(agentsDir, Label+".plist")
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	// 可能已在运行：先 bootout 再 bootstrap（幂等）。
	_ = exec.Command("launchctl", "bootout", domain+"/"+Label).Run()
	out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installSystemd(bin string) error {
	dir := filepath.Join(os.Getenv("HOME"), ".config/systemd/user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Pengshan daemon (Qingniao companion)

[Service]
ExecStart=%s run
Restart=no

[Install]
WantedBy=default.target
`, bin)
	unitPath := filepath.Join(dir, Label+".service")
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	out, err = exec.Command("systemctl", "--user", "enable", "--now", Label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl enable --now: %v: %s", err, out)
	}
	return nil
}
