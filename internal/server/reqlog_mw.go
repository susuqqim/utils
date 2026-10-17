// reqlog_mw.go —— 请求指标采集中间件（S3 第二步，上游 b2a93bb 的本地化）。
//
// 只采集**结构化指标**并统一走 reqlog 的脱敏（IP /24、UA 120、uid 8 位），**绝不落正文**。
// 设计要点：
//   - 只包在**网关请求**上（/v1/*）并在 main 里显式接线，避免把面板自身流量也灌进归档；
//   - 采集不阻塞：写环是纳秒级内存操作；归档写盘走**独立 goroutine + 有界队列**，队列满即丢
//     （丢弃计数记在 Ring 之外，宁缺日志不拖慢请求）；
//   - 模型名从请求体里取（受大小限制保护），失败就不记；
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/reqlog"
)

// reqLogQueue 归档写入队列（有界；满则丢并计数）。
type reqLogQueue struct {
	ch     chan reqlog.Entry
	drops  chan struct{}
	closed chan struct{}
}

// ReqLogSink 接收一条采集结果（可为 nil = 只写内存环）。
type ReqLogSink struct {
	Ring    *reqlog.Ring
	Archive *reqlog.Archive
}

// ReqLogMiddleware 包装 next，采集 /v1/* 的结构化指标交给 sink。
func ReqLogMiddleware(next http.Handler, sink *ReqLogSink) http.Handler {
	if sink == nil || sink.Ring == nil {
		return next
	}
	q := &reqLogQueue{ch: make(chan reqlog.Entry, 1024), closed: make(chan struct{})}
	if sink.Archive != nil {
		go func() {
			for {
				select {
				case e, ok := <-q.ch:
					if !ok {
						return
					}
					_ = sink.Archive.Append(e)
				case <-q.closed:
					return
				}
			}
		}()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var model string
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.Body != nil {
			// 只读少量字节找 "model"（超过 64KB 不读，避免影响大 body 请求）
			orig := r.Body
			if b, err := io.ReadAll(io.LimitReader(orig, 64<<10)); err == nil {
				// **必须把"已读前缀 + 未读剩余"一起交回下游**：只放回前缀会把 >64KB 的请求体
				// 静默截断（大上下文/文件块请求正好会踩到），且下游只会报模糊的解析错误。
				r.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(b), orig), orig}
				var probe struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(b, &probe) == nil {
					model = probe.Model
				}
			}
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			return // 面板/健康检查等：不入指标
		}
		e := reqlog.Entry{
			TS:        start,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    sw.status,
			LatencyMS: time.Since(start).Milliseconds(),
			Model:     model,
			IP:        reqlog.SanitizeIP(clientIPOf(r)),
			UA:        reqlog.SanitizeUA(r.UserAgent()),
		}
		sink.Ring.Add(e)
		if sink.Archive == nil {
			return
		}
		select {
		case q.ch <- e:
		default: // 队列满：丢弃，绝不阻塞请求
		}
	})
}

// statusWriter 记录状态码（不实现 Flusher/Hijacker 透传以外的接口，流式由 http.ResponseWriter 默认行为覆盖）。
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush 透传（SSE 依赖）。
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// clientIPOf 取首个 X-Forwarded-For / X-Real-IP，回落到 RemoteAddr（与 upstream.ExtractClientIP 同口径的简化版）。
func clientIPOf(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
