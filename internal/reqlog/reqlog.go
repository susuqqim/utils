// Package reqlog 请求指标日志（上游 b2a93bb/4118b22/f8bca38 + 7d462bb 的本地化）。
//
// 设计口径（用户批准，§T.34.7 决策③ 默认）：
//   - 内存环 **500** 条（面板即时视图）；
//   - 磁盘归档 NDJSON，**保留 7 天 / ≤50MB**（LRU 式按最旧文件回收）；
//   - 脱敏：IP 截断到 /24（IPv6 到 /48）、UA 截断 120 字符、uid 只留前 8 位；
//   - **不落** prompt / 响应正文（只记结构化指标）；
//   - 读取按**事件时间**排序（上游 7d462bb：不依赖归档文件 mtime）。
package reqlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry 一条请求指标（**不含**请求/响应正文）。
type Entry struct {
	TS        time.Time `json:"ts"`
	Method    string    `json:"method,omitempty"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	Model     string    `json:"model,omitempty"`
	UID       string    `json:"uid,omitempty"` // 前 8 位
	IP        string    `json:"ip,omitempty"`  // 截断到网络段
	UA        string    `json:"ua,omitempty"`  // 截断 120
	Tokens    int       `json:"tokens,omitempty"`
	Credit    float64   `json:"credit,omitempty"`
}

// SanitizeIP 把 IP 截断到网络段：IPv4 保留前 3 段（/24）；IPv6 保留前 3 组（/48）。
// 非 IP（含空串/主机名）原样返回但截断长度，避免异常输入撑爆归档。
func SanitizeIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, ":"); i > 0 { // IPv6（含带端口形式 a::b:port）
		parts := strings.Split(s[:i], ":")
		if len(parts) >= 3 {
			return strings.Join(parts[:3], ":") + "::/48"
		}
		return s
	}
	if i := strings.LastIndex(s, "."); i > 0 {
		parts := strings.Split(s, ".")
		if len(parts) == 4 {
			return strings.Join(parts[:3], ".") + ".0/24"
		}
	}
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// SanitizeUA 去掉控制字符并按 rune 截断到 120 字符（客户端可控，必须设上限）。
func SanitizeUA(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) <= 120 {
		return s
	}
	return string([]rune(s)[:120])
}

// UID8 只保留 uid 前 8 位（与日志口径一致）。
func UID8(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// Ring 定容环形缓冲（并发安全）。
type Ring struct {
	mu   sync.RWMutex
	buf  []Entry
	next int
	full bool
	cap  int
}

// NewRing 新建环形缓冲（cap<=0 取默认 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{buf: make([]Entry, capacity), cap: capacity}
}

// Add 写入一条（覆盖最旧）。
func (r *Ring) Add(e Entry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % r.cap
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot 返回按**时间升序**的副本（最旧在前）。
func (r *Ring) Snapshot() []Entry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.next
	if r.full {
		n = r.cap
	}
	out := make([]Entry, 0, n)
	if r.full {
		out = append(out, r.buf[r.next:]...)
		out = append(out, r.buf[:r.next]...)
	} else {
		out = append(out, r.buf[:r.next]...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// Archive 磁盘归档（NDJSON，按天分文件；保留期 + 容量双上限）。
type Archive struct {
	Dir      string
	Retain   time.Duration // 默认 7 天
	MaxBytes int64         // 默认 50MB
	mu       sync.Mutex
}

// NewArchive 新建归档（零值字段取默认：7 天 / 50MB）。
func NewArchive(dir string, retain time.Duration, maxBytes int64) *Archive {
	if retain <= 0 {
		retain = 7 * 24 * time.Hour
	}
	if maxBytes <= 0 {
		maxBytes = 50 << 20
	}
	return &Archive{Dir: dir, Retain: retain, MaxBytes: maxBytes}
}

func (a *Archive) fileFor(t time.Time) string {
	return filepath.Join(a.Dir, "req-"+t.UTC().Format("2006-01-02")+".ndjson")
}

// Append 追加一条并按需回收（保留期 + 容量）。
func (a *Archive) Append(e Entry) error {
	if a == nil || a.Dir == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(a.fileFor(e.TS), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	a.pruneLocked(time.Now())
	return nil
}

// pruneLocked 先按保留期删旧文件，再按总容量从最旧开始删（调用方持锁）。
func (a *Archive) pruneLocked(now time.Time) {
	names, _ := filepath.Glob(filepath.Join(a.Dir, "req-*.ndjson"))
	sort.Strings(names) // 文件名含日期 ⇒ 字典序 = 时间序
	var total int64
	type fi struct {
		name string
		size int64
		mod  time.Time
	}
	var files []fi
	for _, n := range names {
		st, err := os.Stat(n)
		if err != nil {
			continue
		}
		if now.Sub(st.ModTime()) > a.Retain {
			_ = os.Remove(n)
			continue
		}
		files = append(files, fi{n, st.Size(), st.ModTime()})
		total += st.Size()
	}
	for _, f := range files {
		if total <= a.MaxBytes {
			break
		}
		if err := os.Remove(f.name); err == nil {
			total -= f.size
		}
	}
}

// Read 读取归档并按**事件时间**升序返回（上游 7d462bb：不依赖文件 mtime）。
func (a *Archive) Read(since time.Time, limit int) ([]Entry, error) {
	if a == nil || a.Dir == "" {
		return nil, nil
	}
	names, _ := filepath.Glob(filepath.Join(a.Dir, "req-*.ndjson"))
	sort.Strings(names)
	var out []Entry
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue // 半截行/损坏行：跳过而不是整体失败
			}
			if !since.IsZero() && e.TS.Before(since) {
				continue
			}
			out = append(out, e)
		}
		_ = f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:] // 只要最近的 limit 条（仍按时间升序）
	}
	return out, nil
}

// Stats 归档概览（面板展示用）。
func (a *Archive) Stats() (files int, bytes int64) {
	if a == nil || a.Dir == "" {
		return 0, 0
	}
	names, _ := filepath.Glob(filepath.Join(a.Dir, "req-*.ndjson"))
	for _, n := range names {
		if st, err := os.Stat(n); err == nil {
			files++
			bytes += st.Size()
		}
	}
	return files, bytes
}

// String 便于日志/面板展示一行摘要。
func (e Entry) String() string {
	return fmt.Sprintf("%s %s %s %d %dms", e.TS.Format(time.RFC3339), e.Method, e.Path, e.Status, e.LatencyMS)
}
