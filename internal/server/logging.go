// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start time.Time
	model string
	mode  string // "stream" | "sync"
	uid   string // 完整 uid，展示时只取前 8 位
	ttfb  time.Duration
	toks  int // <0 表示 usage 缺失 → 显示 "-"
	// reasonToks 上游 usage.completion_tokens_details.reasoning_tokens；<0 = 缺失。
	// 为什么要单独记：推理模型的 completion_tokens **包含**推理 token（实测一次请求
	// completion=127 / reasoning=99 / 可见内容仅 28），不拆开看会误以为"输出很多但很慢"。
	reasonToks int
	credit     float64
	hasCredit  bool
	status     int

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1, reasonToks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks, s.reasonToks, s.credit, s.hasCredit)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br    *bufio.Reader
	start time.Time
	// ttfbNano 首个 data 帧到达耗时（纳秒；0=未记录）。
	// 用原子量而非普通字段：SSE 读取协程写入它的同时，客户端取消路径上的 handler
	// 会并发调用 TTFB() 读取（go test -race 实测报警）。
	ttfbNano atomic.Int64

	// mu 保护以下**全部**统计字段。为什么不能只靠"读取结束后才读"的 happens-before：
	// upstream.StreamContext 在**早退路径**（客户端取消、下游写错误、keep-alive 写失败）
	// 会直接 return，**不等**内部那个 SSE 读取协程退出（读取协程可能仍阻塞在上游 body 上，
	// 而 body 是在 StreamContext 返回**之后**才被关闭的，所以也不能改成等它——会死锁）。
	// 此时 handler 紧接着读 Tokens/Credit/TotalTokens/Usage，与写入并发：
	//   - 撕裂读可得 (hasCredit=true, credit=0) → NoteModelCost 记 0 单价 → EMA 被拉低 →
	//     自适应分块误判"极省档"（额度 25 而非 50/100）；
	//   - 这正是 logging.go 里"缺失≠0，不能把缺观测当 0 成本"注释要避免的事。
	mu sync.Mutex
	// upstreamErr 本流是否出现过上游错误帧（见 UpstreamErrorFrame）。
	upstreamErr bool
	seen        bool // 已见过首个 data 帧（TTFB 只记一次；仅读取协程访问）
	hasUsage    bool // 末帧是否带 usage
	hasCredit   bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens      int
	credit      float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt      int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cached      int     // 末帧 usage 缓存命中 prompt tokens
	hasCached   bool
	// reason 末帧 usage.completion_tokens_details.reasoning_tokens（推理 token，
	// 已含在 completion_tokens 内）；hasReason 区分"缺失"与"0"。
	reason    int
	hasReason bool
	pend      []byte // 已读未返回的行缓存（仅 Read 自己访问，不在 mu 保护范围）
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。可被读取协程之外的 goroutine 并发调用。
func (s *chatStatsReader) TTFB() time.Duration { return time.Duration(s.ttfbNano.Load()) }

// UpstreamErrorFrame 报告本流里是否出现过上游错误帧（HTTP 200 + data:{"error":…}）。
// 供成功路径决定"能不能算这一轮成功"（别拿它去清 11102 负缓存）。
func (s *chatStatsReader) UpstreamErrorFrame() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstreamErr
}

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens, s.hasUsage
}

// ReasoningTokens 返回末帧 usage 的推理 token 数（completion_tokens 的子集）；
// ok=false 表示上游未给该字段（缺失≠0，展示时不要写 "0"）。
func (s *chatStatsReader) ReasoningTokens() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.hasReason
}

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credit, s.hasUsage && s.hasCredit
}

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt + s.tokens
}

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasUsage && s.prompt >= 0,
		PromptTokens:        int64(s.prompt),
		HasCompletionTokens: s.hasUsage && s.tokens >= 0,
		CompletionTokens:    int64(s.tokens),
		HasTotalTokens:      s.hasUsage && (s.prompt+s.tokens) >= 0,
		TotalTokens:         int64(s.prompt + s.tokens),
		HasCachedTokens:     s.hasUsage && s.hasCached,
		CachedTokens:        int64(s.cached),
	}
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	if n, ok := upstream.UsageCacheHitTokens(u); ok {
		delta.HasCachedTokens, delta.CachedTokens = true, n
	}
	return delta
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
// 全程持 s.mu：handler 可能在早退路径上与它并发（见 mu 字段的说明）。
func (s *chatStatsReader) parseSSELine(line string) {
	payload, ok := upstream.SSEDataPayload(line)
	if !ok {
		return
	}
	if payload == "[DONE]" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		s.seen = true
		s.ttfbNano.Store(int64(time.Since(s.start)))
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
		} `json:"usage"`
		// Error 上游错误帧（HTTP 200 + data:{"error":…}，如 6004 限流、审核拦截：
		// sse.go 明确把这类帧原样透传并**计入有效帧**）。必须单独识别：否则这条流会被
		// 记成"这一轮成功"（ok=true）→ 成功路径的 BlockModelClear 会把该 (账号,模型)
		// 刚写下的 11102 负缓存误清，保护被自己人撤除（审查 D 的 P2-8）。
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
		s.upstreamErr = true
	}
	if chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	// 缓存命中：真实命中在 prompt_tokens_details.cached_tokens，上游可能同时输出
	// cached_tokens:0 等零值别名；复用 upstream 口径取各处别名的最大值。
	if raw := map[string]any{}; json.Unmarshal([]byte(payload), &raw) == nil {
		if u, ok := raw["usage"].(map[string]any); ok {
			if n, ok := upstream.UsageCacheHitTokens(u); ok {
				s.hasCached = true
				s.cached = int(n)
			}
			// 推理 token（已是 completion_tokens 的子集）：单独记下来，
			// 展示时拆开，避免"输出 127 却只见 28 字"被误读成丢字/慢。
			if n, ok := upstream.UsageReasoningTokens(u); ok {
				s.hasReason = true
				s.reason = int(n)
			}
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
// completionReasoningTokens 取非流式响应 usage 里的推理 token 数；无字段返回 -1。
func completionReasoningTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	if n, ok := upstream.UsageReasoningTokens(u); ok {
		return int(n)
	}
	return -1
}

func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// tokensPerSecond 计算展示用速率（tok/s）。
//
// **口径**：`tokens / (总时长 - 首帧等待)`，即"首字之后的生成期"速率。
// 为什么不用端到端：端到端把提示词处理/排队（TTFB）算进分母，短回答会被严重低估 ——
// 实测同一请求：端到端 58.6 tok/s vs 生成期 173.9 tok/s（**3 倍差**），
// 而 DSH 等客户端按"生成期/可见内容"口径显示，于是面板与客户端数字对不上。
// ttfb<=0（未测到首帧，如非流式）或 ttfb>=总时长（异常）时回退为端到端。
// minGenWindow 可信生成窗口的下限（上游 issue #127 移植）：扣除 TTFB 后剩余窗口
// 不足该值即视为「生成时长不可测」，退回端到端耗时。
//
// 为什么：ttfb 量的是"首个 SSE 帧到达"。当上游把整个响应攒到最后一次性下发
// （假流式 / 中间层攒批刷新——首帧与末帧几乎同时到）时，total−ttfb 只剩几毫秒，
// 拿它当分母会把几百 token 除成上万 tok/s 的幻数。真流式下首帧到末帧通常铺满
// 剩余窗口，200ms 远低于正常生成时长，不影响真实快速输出（如缓存命中后的爆发）。
const minGenWindow = 200 * time.Millisecond

// tokensPerSecondBasis 同 tokensPerSecond，额外返回速率口径：
// "gen" = 生成期（扣除 TTFB 后）；"e2e" = 端到端回退（攒批下发 / 窗口不足 minGenWindow /
// 未测到 TTFB）；"" = 无有效样本。日志行据此标注，避免把端到端口径标成"生成期"。
func tokensPerSecondBasis(toks int, total, ttfb time.Duration) (float64, string) {
	if toks < 0 {
		return 0, ""
	}
	gen := total
	basis := "e2e"
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
			basis = "gen"
		}
	}
	if gen <= 0 {
		return 0, ""
	}
	return float64(toks) / gen.Seconds(), basis
}

func tokensPerSecond(toks int, total, ttfb time.Duration) float64 {
	if toks < 0 {
		return 0
	}
	v, _ := tokensPerSecondBasis(toks, total, ttfb)
	return v
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"；reasonToks 为其中推理 token（<0=缺失）。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks, reasonToks int, credit float64, hasCredit bool) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	// 按 rune 边界截断：模型名由客户端提供，可能是中文/emoji，直接 model[:11] 会把
	// 多字节字符切成半个（终端与 /panel/api/logs 里显示为乱码）。
	model = logfmt.Truncate(model, 11)
	tokField := "-"
	tokpsField := "-"
	basisField := "生成期" // 未回退时口径就是生成期；回退端到端时由 tokensPerSecondBasis 改写
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if reasonToks > 0 && reasonToks <= toks {
			// 拆出推理 token：否则"输出 127 却只看到 28"会被误读成丢字
			tokField = fmt.Sprintf("%d(推%d)", toks, reasonToks)
		}
		if v, basis := tokensPerSecondBasis(toks, total, ttfb); basis != "" {
			tokpsField = fmt.Sprintf("%.1f", v)
			basisField = map[string]string{"gen": "生成期", "e2e": "端到端"}[basis]
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	creditField := "-"
	if hasCredit {
		creditField = fmt.Sprintf("%.2f", credit)
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s(%s) | credit=%s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		basisField,
		creditField,
		total.Seconds(),
	)
}
