package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"dsh/local/screentranslator/internal/engine"
	"dsh/local/screentranslator/internal/logx"
)

// Control 是守护进程的本地控制通道。
//
// 只做一件事：让外壳请求「优雅关闭」，好让 Engine 先释放占用锁再退出。
// 监听 127.0.0.1 的随机端口，端口通过 stdout 的 `CONTROL_PORT=…` 交给外壳。
type Control struct {
	srv  *http.Server
	port int
}

// Port 返回实际监听端口。
func (c *Control) Port() int { return c.port }

// Close 停止监听。
func (c *Control) Close() { _ = c.srv.Close() }

// startControlServer 起一个只监听回环的 HTTP 服务：
// POST /shutdown → 触发 stop（取消 ctx，Engine 收尾后退出）。
func startControlServer(stop context.CancelFunc, logger *logx.Logger, eng *engine.Engine) *Control {
	mux := http.NewServeMux()
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		logger.Info("收到 /shutdown，开始优雅退出")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		// 先回包再取消，保证外壳能收到 200
		go func() {
			time.Sleep(50 * time.Millisecond)
			stop()
		}()
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		state := "unknown"
		if eng != nil {
			state = eng.State()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"state":%q,"region":%q}`, state, eng.RegionMode())
	})
	// 判稳区域模式：fixed（默认）/ auto（在线学习，实验）
	mux.HandleFunc("/region", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			mode := r.URL.Query().Get("mode")
			if mode != "auto" && mode != "fixed" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"ok":false,"error":"mode must be auto or fixed"}`))
				return
			}
			eng.SetRegionMode(mode)
			logger.Info("判稳区域模式切换为 %s", mode)
		}
		fmt.Fprintf(w, `{"ok":true,"region":%q}`, eng.RegionMode())
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// 控制通道起不来不影响主功能：外壳会退化为硬杀
		return &Control{srv: &http.Server{}, port: 0}
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Printf("control server stopped: %v\n", err)
		}
	}()
	return &Control{srv: srv, port: ln.Addr().(*net.TCPAddr).Port}
}
