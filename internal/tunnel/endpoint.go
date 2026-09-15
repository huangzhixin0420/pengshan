// Package tunnel 把一条 WS 连接包装成 net.Conn 兼容的 E2E 加密字节端点：
// 写入侧按 64KB 分块封 e2e 帧，读出侧逐帧解密重组为连续字节流。
// 帧语义见 internal/e2e：data 密文、ping/pong 空明文、close 密文 reason。
package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/huangzhixin0420/pengshan/internal/e2e"
)

// Endpoint 实现 net.Conn。并发模型：Read 与 Write 可各属一个 goroutine
// （与 io.Copy 双向转发匹配）；多个并发 Read 或并发 Write 由调用方互斥
// （bridge 用法天然满足）。
type Endpoint struct {
	ws   *websocket.Conn
	send *e2e.Cipher
	recv *e2e.Cipher

	rmu  sync.Mutex
	rbuf []byte // 已解密待消费缓冲

	wmu sync.Mutex

	closeOnce sync.Once
	done      chan struct{}
	closeErr  error // 终态：首个导致关闭的错误（Read 返回它）
	errMu     sync.Mutex
}

// New 以已完成的握手密钥构造端点。
func New(ws *websocket.Conn, keys *e2e.Keys) *Endpoint {
	ws.SetReadLimit(e2e.ReadLimit)
	return &Endpoint{
		ws:   ws,
		send: e2e.NewCipher(keys.Send, true),
		recv: e2e.NewCipher(keys.Recv, false),
		done: make(chan struct{}),
	}
}

// Read 从隧道读连续字节流（跨帧重组）。
func (e *Endpoint) Read(p []byte) (int, error) {
	e.rmu.Lock()
	defer e.rmu.Unlock()

	for len(e.rbuf) == 0 {
		if e.isClosed() {
			return 0, e.getCloseErr()
		}
		_, data, err := e.ws.Read(context.Background())
		if err != nil {
			e.terminate(err)
			return 0, e.getCloseErr()
		}
		ft, pt, err := e.recv.Open(data)
		if err != nil {
			e.terminate(err)
			return 0, e.getCloseErr()
		}
		switch ft {
		case e2e.FramePing:
			// 应用层 ping 由外层（心跳循环）消费；此端点语义下忽略并继续。
			continue
		case e2e.FrameClose:
			e.terminate(io.EOF)
			if len(pt) > 0 {
				e.setCloseErr(io.EOF) // reason 可记日志；Read 语义返回 EOF
			}
			return 0, e.getCloseErr()
		case e2e.FrameData:
			e.rbuf = append(e.rbuf, pt...)
		default: // pong/err：端点语义不消费，忽略
			continue
		}
	}
	n := copy(p, e.rbuf)
	e.rbuf = e.rbuf[n:]
	return n, nil
}

// Write 把 p 按 64KB 分块封帧写入。
func (e *Endpoint) Write(p []byte) (int, error) {
	e.wmu.Lock()
	defer e.wmu.Unlock()

	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 64*1024 {
			chunk = chunk[:64*1024]
		}
		sealed, err := e.send.Seal(e2e.FrameData, chunk)
		if err != nil {
			return total, err
		}
		if err := e.ws.Write(context.Background(), websocket.MessageText, sealed); err != nil {
			e.terminate(err)
			return total, e.getCloseErr()
		}
		p = p[len(chunk):]
		total += len(chunk)
	}
	return total, nil
}

// Close 发 close 帧（best effort）并关 WS。幂等。
func (e *Endpoint) Close() error {
	e.closeOnce.Do(func() {
		// 尽力通知对端；ws 可能已坏，忽略错误。
		if sealed, err := e.send.Seal(e2e.FrameClose, []byte("local close")); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = e.ws.Write(ctx, websocket.MessageText, sealed)
			cancel()
		}
		_ = e.ws.CloseNow()
		e.terminate(io.EOF)
	})
	return nil
}

func (e *Endpoint) terminate(err error) {
	e.errMu.Lock()
	if e.closeErr == nil {
		e.closeErr = err
	}
	e.errMu.Unlock()
	select {
	case <-e.done:
	default:
		close(e.done)
	}
}

func (e *Endpoint) isClosed() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

func (e *Endpoint) getCloseErr() error {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	if e.closeErr == nil {
		return io.EOF
	}
	return e.closeErr
}

func (e *Endpoint) setCloseErr(err error) {
	e.errMu.Lock()
	e.closeErr = err
	e.errMu.Unlock()
}

// 以下 net.Conn 接口的 Addr/Deadline 在蓬山用法（bridge io.Copy）中不被消费：
// 隧道是抽象字节管道，没有有意义的本地/对端地址；WS 层的超时由外层管理。
func (e *Endpoint) LocalAddr() net.Addr                { return pipeAddr("tunnel-local") }
func (e *Endpoint) RemoteAddr() net.Addr               { return pipeAddr("tunnel-remote") }
func (e *Endpoint) SetDeadline(_ time.Time) error      { return errNoDeadline }
func (e *Endpoint) SetReadDeadline(_ time.Time) error  { return errNoDeadline }
func (e *Endpoint) SetWriteDeadline(_ time.Time) error { return errNoDeadline }

var errNoDeadline = errors.New("tunnel: deadlines not supported on e2e endpoint")

type pipeAddr string

func (a pipeAddr) Network() string { return "pengshan-tunnel" }
func (a pipeAddr) String() string  { return string(a) }
