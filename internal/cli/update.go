// update 命令：从 GH Release 自更新。
// 流程：目标 release（--version 或 latest）→ 匹配平台资产 → sha256 校验
//（checksums.txt 优先，缺失则报 warn）→ 解压到临时目录 → 备份当前二进制
// → rename 替换（inode 语义，运行中的进程不受影响）→ 若 launchd 管理则
// kickstart -k 重启加载新二进制，否则提示手动重启。
package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/huangzhixin0420/pengshan/internal/autostart"
	"github.com/huangzhixin0420/pengshan/internal/version"
)

const releasesAPI = "https://api.github.com/repos/huangzhixin0420/pengshan/releases"

func newUpdateCmd() *cobra.Command {
	var wantVersion string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "检查并应用最新版本（GH Release，sha256 校验后自替换）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(wantVersion, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&wantVersion, "version", "v", "", "指定版本如 v0.1.0（默认 latest）")
	return cmd
}

type releaseInfo struct {
	TagName string          `json:"tag_name"`
	Assets  []assetInfo     `json:"assets"`
}

type assetInfo struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func runUpdate(wantVersion string, out io.Writer) error {
	client := &http.Client{Timeout: 30 * time.Second}

	rel, err := fetchRelease(client, wantVersion)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "目标版本：%s（当前 %s）\n", rel.TagName, version.Version)
	if rel.TagName == version.Version {
		fmt.Fprintln(out, "已是最新，无需更新。")
		return nil
	}

	assetName := fmt.Sprintf("pengshan-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	checksums, assetURL, err := findAssets(rel, assetName)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "pengshan-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// 1) 下载资产 + checksums，校验。
	tgzPath := filepath.Join(tmp, assetName)
	if err := download(client, assetURL, tgzPath); err != nil {
		return fmt.Errorf("下载 %s: %w", assetName, err)
	}
	sumPath := filepath.Join(tmp, "checksums.txt")
	if err := download(client, checksums, sumPath); err != nil {
		return fmt.Errorf("下载 checksums.txt: %w", err)
	}
	if err := verifyChecksum(sumPath, assetName, tgzPath); err != nil {
		return fmt.Errorf("校验失败: %w（已中止，未改动现有二进制）", err)
	}
	fmt.Fprintln(out, "✓ 校验通过")

	// 2) 解包到临时目录。
	extractDir := filepath.Join(tmp, "x")
	if err := extractTarGz(tgzPath, extractDir); err != nil {
		return err
	}

	// 3) 替换自身：当前可执行文件路径（os.Executable 会解析 symlink——
	//    install.sh 安装的是真实文件，无 symlink 问题）。
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	newBin := filepath.Join(extractDir, "pengshan")
	newRelay := filepath.Join(extractDir, "pengshan-relay")

	if err := replaceFile(newBin, self); err != nil {
		return err
	}
	// relay 二进制同目录（若存在则一并换）。
	if _, err := os.Stat(self + "-relay"); err == nil {
		if err := replaceFile(newRelay, self+"-relay"); err != nil {
			fmt.Fprintf(out, "⚠ relay 替换失败（不影响 daemon）：%v\n", err)
		}
	}
	fmt.Fprintf(out, "✓ 已替换 %s\n", self)

	// 4. 重启加载新二进制。
	if autostart.Installed() {
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		if kb, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+autostart.Label).CombinedOutput(); err != nil {
			fmt.Fprintf(out, "⚠ kickstart 失败（手动: launchctl kickstart -k %s/%s）：%v %s\n", domain, autostart.Label, err, kb)
		} else {
			fmt.Fprintln(out, "✓ launchd 已重启 daemon")
		}
	} else {
		fmt.Fprintln(out, "daemon 未由 launchd 管理：请手动重启运行中的 pengshan run 进程。")
	}
	fmt.Fprintln(out, "更新完成："+rel.TagName)
	return nil
}

// fetchRelease 按 tag 或 latest 取 release 元数据。
func fetchRelease(client *http.Client, wantVersion string) (*releaseInfo, error) {
	url := releasesAPI + "/latest"
	if wantVersion != "" {
		url = releasesAPI + "/tags/" + wantVersion
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pengshan-update")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查 release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("release 不存在: %s", wantVersion)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API HTTP %d", resp.StatusCode)
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("release 无 tag")
	}
	return &rel, nil
}

// findAssets 找平台资产与 checksums 的下载地址。
func findAssets(rel *releaseInfo, assetName string) (checksumsURL, assetURL string, err error) {
	for _, a := range rel.Assets {
		switch a.Name {
		case "checksums.txt":
			checksumsURL = a.BrowserDownloadURL
		case assetName:
			assetURL = a.BrowserDownloadURL
		}
	}
	if assetURL == "" {
		return "", "", fmt.Errorf("release %s 无资产 %s（可用资产见 release 页）", rel.TagName, assetName)
	}
	if checksumsURL == "" {
		return "", "", fmt.Errorf("release %s 缺 checksums.txt（拒绝无校验更新）", rel.TagName)
	}
	return checksumsURL, assetURL, nil
}

func download(client *http.Client, url, dest string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "pengshan-update")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// verifyChecksum 从 checksums.txt 找目标行的 sha256 并比对。
func verifyChecksum(sumPath, assetName, tgzPath string) error {
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "./") == assetName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt 无 %s 条目", assetName)
	}
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("sha256 不符: got %s want %s", got, want)
	}
	return nil
}

// extractTarGz 解 tar.gz 到目录。
func extractTarGz(tgzPath, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// 防路径穿越。
		name := filepath.Clean(hdr.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("非法归档路径: %s", hdr.Name)
		}
		target := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeReg:
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

// replaceFile 备份 dst 为 .bak 再原子 rename 新文件。
func replaceFile(src, dst string) error {
	backup := dst + ".bak"
	os.Remove(backup)
	if err := os.Rename(dst, backup); err != nil {
		return fmt.Errorf("备份 %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		// 尽力回滚。
		_ = os.Rename(backup, dst)
		return fmt.Errorf("替换 %s: %w", dst, err)
	}
	_ = os.Chmod(dst, 0o755)
	return nil
}
