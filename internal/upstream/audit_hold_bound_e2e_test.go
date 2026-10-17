package upstream

// audit_hold_bound_e2e_test.go —— 独立对抗审计（红队）用例。
//
// 本文件**不复用** sse_markup_wire_test.go 的断言工具：两处都从导出入口
// StreamContextOpt + httptest.ResponseRecorder 直接跑真实的上游 SSE 字节，
// 目的是在"网关之外"复算两条不变量，而不是相信既有用例的结论。
//
// A) 缓冲上界（批次42 P1 修复）端到端：
//    上游先发 markupOpen，随后是 10MB 永不闭合的正文（全程不出现 markupClose），
//    按 8KB 一帧喂入 —— 真实上游就是按帧切分的；只有多帧才会走到 hold 的增量累积
//    路径，单帧 10MB 会命中"入参即超限"的短路分支，把真正要审计的累积逻辑掩盖掉。
//    断言：正文 10MB 一字节不少、不产生 tool_calls、不 panic、< 60s、Alloc 增量 < 200MB。
//
// B) 未启用时逐字节零改写（独立复核）：
//    同一段输入分别在
//      ① 不传任何选项、
//      ② WithMarkupRepair(nil)、
//      ③ WithMarkupRepair(NewMarkupRepair(map{"不存在的工具":true}, false))、
//      ④ WithMarkupRepair(NewMarkupRepair(nil, false))（真正未启用）
//    四种形态下跑，逐字节比较。③ 是需求里点名的"未启用"形态，但它其实
//    Enabled()==true（allow-list 非空即启用）；本用例因此把 ④ 也纳入比较，并用
//    "流尾未闭合块"探针反证 ③ 会改写输出 —— ③ 不能代表未启用态。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	auditBodyBytes  = 10 << 20 // A) 的正文规模：10MB
	auditFrameBytes = 8 << 10  // 上游 SSE 单帧正文大小（真实量级）
)

// auditChunk 拼一帧上游 chunk（单 choice、index=0），形态与真实上游一致。
func auditChunk(delta map[string]any, finish string) string {
	choice := map[string]any{"index": float64(0), "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	obj := map[string]any{
		"id":      "chatcmpl-audit",
		"object":  "chat.completion.chunk",
		"created": float64(1),
		"model":   "audit-model",
		"choices": []any{choice},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err) // 夹具自身的问题，不是被测行为
	}
	return "data: " + string(b) + "\n\n"
}

// auditRun 用导出入口跑一遍流，返回出站字节。
func auditRun(t *testing.T, in string, opts ...StreamOption) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := StreamContextOpt(context.Background(), rec, strings.NewReader(in), false, opts...); err != nil {
		t.Fatalf("StreamContextOpt 返回错误: %v", err)
	}
	return rec.Body.Bytes()
}

// auditContent 从出站 SSE 还原正文（拼接所有 delta.content），并统计带
// delta.tool_calls 的帧数。只解析最终写出的字节，不信任任何中间态。
func auditContent(t *testing.T, body []byte) (string, int) {
	t.Helper()
	var sb strings.Builder
	callFrames := 0
	for _, line := range strings.Split(string(body), "\n") {
		payload, ok := SSEDataPayload(line)
		if !ok || payload == "[DONE]" {
			continue
		}
		var fr struct {
			Choices []struct {
				Delta map[string]json.RawMessage `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &fr); err != nil {
			t.Fatalf("出站帧不是合法 JSON: %q (%v)", payload, err)
		}
		for _, c := range fr.Choices {
			if raw, ok := c.Delta["content"]; ok {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil {
					sb.WriteString(s)
				}
			}
			if _, ok := c.Delta["tool_calls"]; ok {
				callFrames++
			}
		}
	}
	return sb.String(), callFrames
}

// auditFirstDiff 返回两段字节的首个差异下标（完全相同时返回 -1）。
func auditFirstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// TestAuditHoldBoundE2E 是 A) 的端到端红队用例。
func TestAuditHoldBoundE2E(t *testing.T) {
	// --- 构造上游流：markupOpen + 10MB 连续 x，永不闭合 -----------------------
	var in strings.Builder
	in.Grow(auditBodyBytes + 1<<20)
	in.WriteString(auditChunk(map[string]any{"role": "assistant"}, ""))
	in.WriteString(auditChunk(
		map[string]any{"content": markupOpen + strings.Repeat("x", auditFrameBytes)}, ""))
	for written := auditFrameBytes; written < auditBodyBytes; written += auditFrameBytes {
		in.WriteString(auditChunk(map[string]any{"content": strings.Repeat("x", auditFrameBytes)}, ""))
	}
	in.WriteString(auditChunk(map[string]any{}, "stop"))
	in.WriteString("data: [DONE]\n\n")
	raw := in.String()
	wantBody := strings.Repeat("x", auditBodyBytes) // 期望正文（10MB 个 x，无标记、无他字节）

	repair := NewMarkupRepair(nil, true)
	if !repair.Enabled() {
		t.Fatal("前置失败：NewMarkupRepair(nil, true) 应处于启用状态")
	}

	// --- 基线内存（输入与期望正文都已构造完毕并保持存活）---------------------
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	type auditOutcome struct {
		body []byte
		err  error
		pan  any
	}
	done := make(chan auditOutcome, 1)
	t0 := time.Now()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- auditOutcome{pan: p}
			}
		}()
		rec := httptest.NewRecorder()
		err := StreamContextOpt(context.Background(), rec, strings.NewReader(raw), false,
			WithMarkupRepair(repair))
		done <- auditOutcome{body: rec.Body.Bytes(), err: err}
	}()

	var out auditOutcome
	select {
	case out = <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("A 失败：10MB 未闭合标记流 60s 内未完成（疑似无界缓冲或死循环）")
	}
	dt := time.Since(t0)

	runtime.GC()
	runtime.ReadMemStats(&m1)
	allocDelta := int64(m1.Alloc) - int64(m0.Alloc)
	totalDelta := int64(m1.TotalAlloc) - int64(m0.TotalAlloc)

	if out.pan != nil {
		t.Fatalf("A 失败：流式路径 panic: %v", out.pan)
	}
	if out.err != nil {
		t.Fatalf("A 失败：StreamContextOpt 返回错误: %v", out.err)
	}

	// --- ④ 时间与内存 -------------------------------------------------------
	if dt > 60*time.Second {
		t.Errorf("A 失败：耗时 %v > 60s", dt)
	}
	const allocCap = 200 << 20
	if allocDelta >= allocCap {
		t.Errorf("A 失败：Alloc 增量 %.2fMB 超过 200MB 上限（疑似随输入膨胀）",
			float64(allocDelta)/(1<<20))
	}

	// --- ② 不得产生任何 tool_calls ------------------------------------------
	if bytes.Contains(out.body, []byte("tool_calls")) {
		t.Errorf("A 失败：未闭合标记流出现了 tool_calls 字样")
	}
	text, callFrames := auditContent(t, out.body)
	if callFrames != 0 {
		t.Errorf("A 失败：出站 delta.tool_calls 帧 %d 个，want 0", callFrames)
	}

	// --- ① 正文 10MB 一字节不少 ---------------------------------------------
	if n := strings.Count(text, "x"); n != auditBodyBytes {
		t.Errorf("A 失败：正文 x 字节数 %d，want %d（差 %d）",
			n, auditBodyBytes, auditBodyBytes-n)
	}
	// 定位 10MB x 正文在透出文本里的起点：允许它前面是 markupOpen（某个实现保留
	// 起始标记也合法），但不允许出现别的字节；x 正文本身必须与原输入逐字节一致。
	idx := strings.Index(text, strings.Repeat("x", 64))
	if idx < 0 {
		t.Fatalf("A 失败：透出正文里找不到连续的 x（len=%d）", len(text))
	}
	prefix, bodyText := text[:idx], text[idx:]
	if prefix != "" && prefix != markupOpen {
		t.Errorf("A 失败：x 正文之前出现未知前缀 %q", prefix)
	}
	if bodyText != wantBody {
		t.Errorf("A 失败：正文被改写（%d 字节，want %d；前缀 %d 字节）",
			len(bodyText), len(wantBody), len(prefix))
	}

	t.Logf("A) 耗时=%v；输入 %d 字节 / 出站 %d 字节；正文=%d 字节（x=%d）；"+
		"Alloc增量=%.2fMB；TotalAlloc增量=%.2fMB；tool_calls帧=0；"+
		"修复器收尾：Enabled=%v Seen=%d Converted=%d Rejected=%d hold=%d inBlock=%v",
		dt, len(raw), len(out.body), len(bodyText), strings.Count(bodyText, "x"),
		float64(allocDelta)/(1<<20), float64(totalDelta)/(1<<20),
		repair.Enabled(), repair.Seen(), repair.Converted(), repair.Rejected(),
		len(repair.hold), repair.inBlock)

	// --- 附带发现（P2，非本用例断言项）：give-up 路径丢 markupOpen ------------
	// 触发上限时解析器已经消费掉 markupOpen，但放弃分支只回吐 hold+text，没有把
	// 起始标记补回来；Flush 又因 Enabled()==false 直接返回空。于是"上游发了什么就
	// 透出什么 / 一个字节都不丢"（dsml.go Feed/Flush 注释）在这条路径上不成立。
	// 本用例只留证据、不设门禁（是否要修由产品决策，审计不改生产代码）。
	if prefix == "" {
		probe := NewMarkupRepair(nil, true)
		first := markupOpen + strings.Repeat("y", 16<<10)
		second := strings.Repeat("z", 64<<10)
		e1, c1 := probe.Feed(first)
		e2, c2 := probe.Feed(second)
		got := len(e1) + len(e2) + len(probe.Flush())
		want := len(first) + len(second)
		t.Logf("A) 附带发现(P2)：give-up 丢了起始标记 —— Feed 探针入参 %d 字节，"+
			"透出 %d 字节，差 %d（len(markupOpen)=%d，calls=%d/%d，Enabled=%v）。"+
			"根因：超限分支 return s 时未补回已消费的 markupOpen；Flush 因 Enabled=false 失效",
			want, got, want-got, len(markupOpen), len(c1), len(c2), probe.Enabled())
	}
}

// TestAuditZeroRewrite 是 B) 的独立复核用例。
func TestAuditZeroRewrite(t *testing.T) {
	prose := "先确认仓库状态，再决定动不动代码。\n"
	// 完整块：起止标记落在同一帧内；工具名 Bash 不在 ③ 的 allow-list 里 → 必被拒绝。
	block := mkBlock(mkInvoke("Bash", mkParam("command", "git status --short")))
	in := auditChunk(map[string]any{"role": "assistant"}, "") +
		auditChunk(map[string]any{"content": prose + block + "收尾。"}, "") +
		auditChunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"

	named := NewMarkupRepair(map[string]bool{"不存在的工具": true}, false) // 需求里的"未启用"形态
	reallyOff := NewMarkupRepair(nil, false)                         // 真正未启用
	base := auditRun(t, in)                                          // ①
	withNil := auditRun(t, in, WithMarkupRepair(nil))                // ②
	withNamed := auditRun(t, in, WithMarkupRepair(named))            // ③
	withOff := auditRun(t, in, WithMarkupRepair(reallyOff))          // ④

	var nilRepair *MarkupRepair
	t.Logf("B) Enabled(): ②WithMarkupRepair(nil)=%v ③map{不存在的工具:true}=%v ④NewMarkupRepair(nil,false)=%v"+
		"（③ 实为启用态；本轮 Seen=%d Rejected=%d Converted=%d，说明它真的跑了并拒绝了该块）",
		nilRepair.Enabled(), named.Enabled(), reallyOff.Enabled(),
		named.Seen(), named.Rejected(), named.Converted())

	failBytes := func(name string, want, got []byte) {
		t.Helper()
		if bytes.Equal(want, got) {
			return
		}
		t.Errorf("B 失败：%s 与 ① 不是逐字节相同（①=%d 字节，%s=%d 字节，首处差异 @%d）",
			name, len(want), name, len(got), auditFirstDiff(want, got))
	}
	failBytes("② WithMarkupRepair(nil)", base, withNil)
	failBytes("③ WithMarkupRepair(map{不存在的工具:true},false)", base, withNamed)
	failBytes("④ WithMarkupRepair(NewMarkupRepair(nil,false))", base, withOff)

	for name, b := range map[string][]byte{
		"① 不传选项":                              base,
		"② WithMarkupRepair(nil)":             withNil,
		"③ map{不存在的工具:true}":                  withNamed,
		"④ NewMarkupRepair(nil,false)（真正未启用）": withOff,
	} {
		if bytes.Contains(b, []byte("tool_calls")) {
			t.Errorf("B 失败：%s 输出含 tool_calls", name)
		}
	}

	// --- 决定性探针：流尾仍未闭合的块 ---------------------------------------
	// 这是上游截断/超长块的现实形态。启用态必然改写（标记被 hold 吞掉，流尾由
	// Flush 另起一帧回吐），真正未启用态则一个字节都不动。用它反证 ③ 不是"未启用"。
	partial := markupOpen + "\n" + "<" + markupPipe + "DSML" + markupPipe + ` invoke name="Bash">` + "\n" + "ls -la"
	probeIn := auditChunk(map[string]any{"role": "assistant"}, "") +
		auditChunk(map[string]any{"content": prose + partial}, "") +
		auditChunk(map[string]any{"content": "\n"}, "") +
		auditChunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
	pBase := auditRun(t, probeIn)
	pNil := auditRun(t, probeIn, WithMarkupRepair(nil))
	pNamed := auditRun(t, probeIn, WithMarkupRepair(NewMarkupRepair(map[string]bool{"不存在的工具": true}, false)))
	pOff := auditRun(t, probeIn, WithMarkupRepair(NewMarkupRepair(nil, false)))

	if !bytes.Equal(pBase, pNil) {
		t.Errorf("B 失败：探针下 ② 改写了输出（%d vs %d 字节）", len(pBase), len(pNil))
	}
	if !bytes.Equal(pBase, pOff) {
		t.Errorf("B 失败：探针下 ④（真正未启用）改写了输出（%d vs %d 字节）", len(pBase), len(pOff))
	}
	if bytes.Equal(pBase, pNamed) {
		t.Errorf("B 探针反证失败：③ 对未闭合块也零改写，但 Enabled() 应为 true —— 说明修复层没接上")
	}
	for name, b := range map[string][]byte{
		"探针①": pBase, "探针②": pNil, "探针③": pNamed, "探针④": pOff,
	} {
		if bytes.Contains(b, []byte("tool_calls")) {
			t.Errorf("B 失败：%s 输出含 tool_calls", name)
		}
	}
	t.Logf("B) 探针（流尾未闭合块）：①=%d ②=%d ③=%d ④=%d 字节；③-①=%+d 字节，首处差异 @%d"+
		" → ③ 确实启用了修复层并改写输出，'未启用'只能由 ②/④ 代表",
		len(pBase), len(pNil), len(pNamed), len(pOff),
		len(pNamed)-len(pBase), auditFirstDiff(pBase, pNamed))
}
