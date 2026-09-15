// Package bridge 把隧道端点反向代理到本地 hermes serve：
// 隧道内是 HTTP/1.1 + WS 升级的字节流（app 侧 transport 写入），
// 这里只做 net.Dial + 双向 io.Copy，零协议解析（DEV-PLAN §2.1）。
package bridge

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ServeUnavailableError 是 serve 不可达时的错误前缀（app 侧可见 "serve_unavailable"）。
type ServeUnavailableError struct {
	Addr string
	Err  error
}

func (e *ServeUnavailableError) Error() string {
	return fmt.Sprintf("serve_unavailable %s: %v", e.Addr, e.Err)
}

// Probe 检查本地 serve 的 /api/health（2s 超时）。
func Probe(serveAddr string) error {
	conn, err := net.DialTimeout("tcp", serveAddr, 2*time.Second)
	if err != nil {
		return &ServeUnavailableError{Addr: serveAddr, Err: err}
	}
	_ = conn.Close()
	return nil
}

// Run 把隧道端点桥接到 serve 地址，阻塞到任一个方向结束。
// 一个方向 EOF → 两端全关（粗暴但够用；半关闭细化留 M5）。
func Run(tun net.Conn, serveAddr string, logger *slog.Logger) error {
	if err := Probe(serveAddr); err != nil {
		return err
	}
	upstream, err := net.DialTimeout("tcp", serveAddr, 5*time.Second)
	if err != nil {
		return &ServeUnavailableError{Addr: serveAddr, Err: err}
	}

	var once sync.Once
	closeBoth := func() {
		_ = tun.Close()
		_ = upstream.Close()
		if logger != nil {
			logger.Debug("bridge closed", "serve", serveAddr)
		}
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, tun)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(tun, upstream)
		done <- struct{}{}
	}()
	<-done
	once.Do(closeBoth)
	return nil
}
