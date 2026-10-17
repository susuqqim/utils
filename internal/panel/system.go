package panel

import (
	"log"
	"net/http"
	"time"
	"workbuddy2api/internal/gosafe"
)

// systemRestart 触发网关服务重启（异步执行，先返回 200 OK 响应）。
func (p *Panel) systemRestart(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Restart == nil {
		writeErr(w, http.StatusNotImplemented, "restart_not_supported")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "服务正在重启，请稍候刷新页面...",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go gosafe.Do("panel restart", func() {
		time.Sleep(300 * time.Millisecond)
		if err := p.cfg.Restart(); err != nil {
			log.Printf("[panel] 重启服务失败: %v", err)
		}
	})
}

// systemShutdown 触发网关服务关闭（异步执行，先返回 200 OK 响应）。
func (p *Panel) systemShutdown(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Shutdown == nil {
		writeErr(w, http.StatusNotImplemented, "shutdown_not_supported")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "服务正在关闭...",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go gosafe.Do("panel shutdown", func() {
		time.Sleep(300 * time.Millisecond)
		if err := p.cfg.Shutdown(); err != nil {
			log.Printf("[panel] 关闭服务失败: %v", err)
		}
	})
}
