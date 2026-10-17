// sse_markup_test.go 覆盖「流式路径把正文里的模型原生工具调用标记还原成 delta.tool_calls」
// 这条接线（sse.go 的 WithMarkupRepair / repairFrame / emitChunk / 流尾收尾），见 dsml.go。
//
// 本文件守四条不变量，每条都有对应用例：
//  1. 未挂修复器 / 修复器未启用（客户端没声明过 tools）→ 输出与接线前**逐字节一致**，
//     且绝不产出 tool_calls；
//  2. 只碰 delta.content，reasoning_content 一个字节都不动，也不参与修复；
//  3. 一旦本流还原出工具调用，finish_reason 必须是 tool_calls、只出现一次、
//     且位置在调用帧之后；
//  4. Feed 返回空正文且无调用时删掉 content 键，不产生 `"content":""` 的空内容帧。
package upstream

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// markupRunStream 用 json.Marshal 拼上游 SSE 报文（data: 行 + 空行），末尾补 [DONE]。
// 不手写转义：分隔符是全角字符，字面量容易被编辑器/复制粘贴改坏（同 dsml_test.go 的理由）。
func markupRunStream(frames ...map[string]any) string {
	var sb strings.Builder
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			panic(err)
		}
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// markupChunk 拼一个单 choice（index=0）的流式 chunk。finish 为空则不带 finish_reason 键。
func markupChunk(delta map[string]any, finish string) map[string]any {
	c := map[string]any{"index": float64(0), "delta": delta}
	if finish != "" {
		c["finish_reason"] = finish
	}
	return map[string]any{
		"id":      "x1",
		"object":  "chat.completion.chunk",
		"created": float64(1),
		"model":   "m",
		"choices": []any{c},
	}
}

// markupRunBody 挂上启用的修复器跑一遍流，返回响应体原文。
func markupRunBody(t *testing.T, raw string, allowed map[string]bool) string {
	t.Helper()
	rec := httptest.NewRecorder()
	err := StreamContextOpt(context.Background(), rec, strings.NewReader(raw), false,
		WithMarkupRepair(NewMarkupRepair(allowed, false)))
	if err != nil {
		t.Fatalf("StreamContextOpt: %v", err)
	}
	return rec.Body.String()
}

// markupFrames 汇总一个流里关心的三类事实：正文增量、调用帧下标、收尾帧下标/取值。
type markupFrames struct {
	content            strings.Builder
	toolFrameAt        []int
	finishAt           []int
	finishValue        []string
	emptyContentFrames int
}

func markupScan(t *testing.T, frames []map[string]any) markupFrames {
	t.Helper()
	var p markupFrames
	for i, fr := range frames {
		chs, ok := fr["choices"].([]any)
		if !ok || len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if v, has := d["content"]; has {
				s, _ := v.(string)
				if s == "" {
					p.emptyContentFrames++
				} else {
					p.content.WriteString(s)
				}
			}
			if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
				p.toolFrameAt = append(p.toolFrameAt, i)
			}
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" {
			p.finishAt = append(p.finishAt, i)
			p.finishValue = append(p.finishValue, v)
		}
	}
	return p
}

// 用例①：真实泄漏素材（testdata/leaked_markup.txt）经流式路径还原成 delta.tool_calls，
// 正文不残留标记；同时守住不变量 3（finish_reason 恰好一次、值为 tool_calls、在调用帧之后）。
func TestStreamMarkupRepairRestoresTestdataToolCalls(t *testing.T) {
	rawBytes, err := os.ReadFile("testdata/leaked_markup.txt")
	if err != nil {
		t.Fatal(err)
	}
	text := string(rawBytes)
	cut := strings.Index(text, markupOpen)
	if cut < 0 {
		t.Fatalf("夹具里没有标记开始标记")
	}
	// 刻意切在开始标记**内部**：上游把标记切成两片是常态，跨帧缓冲必须能拼回来。
	cut += len(markupOpen) - 2

	raw := markupRunStream(
		markupChunk(map[string]any{"role": "assistant"}, ""),
		markupChunk(map[string]any{"content": text[:cut]}, ""),
		// 这一帧同时带上游重复贴的 finish_reason：必须被摘掉，不能抢在调用帧之前发。
		markupChunk(map[string]any{"content": text[cut:]}, "stop"),
		markupChunk(map[string]any{}, "stop"),
	)
	body := markupRunBody(t, raw, map[string]bool{"exec_command": true})
	frames := framesFromBody(t, body)
	p := markupScan(t, frames)

	if strings.Contains(p.content.String(), markupPipe) || strings.Contains(p.content.String(), "DSML") {
		t.Errorf("正文残留标记: %q", p.content.String())
	}
	if !strings.Contains(p.content.String(), "我先看一下仓库结构和图片内容") {
		t.Errorf("标记之外的正文被吞了: %q", p.content.String())
	}
	if p.emptyContentFrames != 0 {
		t.Errorf("不应出现空内容帧，实际 %d 个", p.emptyContentFrames)
	}
	if len(p.toolFrameAt) != 2 {
		t.Fatalf("应还原 2 个 delta.tool_calls 帧，实际 %d（共 %d 帧）", len(p.toolFrameAt), len(frames))
	}

	// 不变量 3：恰好一个 finish_reason，值为 tool_calls，且排在全部调用帧之后。
	if len(p.finishAt) != 1 || p.finishValue[0] != "tool_calls" {
		t.Fatalf("finish_reason 应恰好一次且为 tool_calls，实际 %v @帧%v", p.finishValue, p.finishAt)
	}
	if last := p.toolFrameAt[len(p.toolFrameAt)-1]; p.finishAt[0] < last {
		t.Errorf("finish_reason 在帧%d，必须排在调用帧(最后 %d)之后", p.finishAt[0], last)
	}

	// 还原出来的调用本身：工具名/参数/各自独立的 index。
	var names []string
	seenIdx := map[float64]bool{}
	for _, fi := range p.toolFrameAt {
		c := frames[fi]["choices"].([]any)[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		tc := d["tool_calls"].([]any)[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		names = append(names, fn["name"].(string))
		if tc["type"] != "function" {
			t.Errorf("帧%d 调用 type 应为 function，实得 %v", fi, tc["type"])
		}
		if idx, ok := tc["index"].(float64); ok {
			if seenIdx[idx] {
				t.Errorf("两个调用复用了同一个 index %v", idx)
			}
			seenIdx[idx] = true
		}
		if id, _ := tc["id"].(string); id == "" {
			t.Errorf("帧%d 调用缺 id", fi)
		}
		if args, _ := fn["arguments"].(string); !strings.Contains(args, "cmd") {
			t.Errorf("帧%d 参数丢失: %q", fi, args)
		}
	}
	if names[0] != "exec_command" || names[1] != "exec_command" {
		t.Errorf("工具名还原错误: %v", names)
	}
}

// 用例②：不变量 1 —— 未挂修复器 / 传 nil / 传未启用的修复器，三种输入产出与「未接线」
// 逐字节一致，且绝不出现 tool_calls（标记原样落在正文里）。
func TestStreamMarkupRepairDisabledIsByteIdentical(t *testing.T) {
	full := "我先看一下仓库结构，再定位价格同步逻辑。\n\n" +
		mkBlock(mkInvoke("exec_command", mkParam("cmd", "git status --short")))
	raw := markupRunStream(
		markupChunk(map[string]any{"role": "assistant"}, ""),
		markupChunk(map[string]any{"content": full}, ""),
		markupChunk(map[string]any{}, "stop"),
	)
	ctx := context.Background()

	recPlain := httptest.NewRecorder()
	if err := Stream(recPlain, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	want := recPlain.Body.String()

	recNil := httptest.NewRecorder()
	if err := StreamContextOpt(ctx, recNil, strings.NewReader(raw), false, WithMarkupRepair(nil)); err != nil {
		t.Fatal(err)
	}
	recOff := httptest.NewRecorder()
	// 客户端没声明过 tools → NewMarkupRepair(nil,false).Enabled() == false
	if err := StreamContextOpt(ctx, recOff, strings.NewReader(raw), false,
		WithMarkupRepair(NewMarkupRepair(nil, false))); err != nil {
		t.Fatal(err)
	}

	if recNil.Body.String() != want {
		t.Errorf("WithMarkupRepair(nil) 必须与未接线输出逐字节一致\nwant %q\ngot  %q", want, recNil.Body.String())
	}
	if recOff.Body.String() != want {
		t.Errorf("未启用的修复器必须与未接线输出逐字节一致\nwant %q\ngot  %q", want, recOff.Body.String())
	}
	if strings.Contains(want, "tool_calls") {
		t.Errorf("未启用时不得产出 tool_calls: %q", want)
	}
	if !strings.Contains(want, markupPipe) {
		t.Errorf("未启用时标记原文必须原样透出: %q", want)
	}
}

// 用例③：不变量 2 —— reasoning_content 原样透出，且**不参与修复**：
// 标记只出现在思维链里时不得被还原成调用。
func TestStreamMarkupRepairNeverTouchesReasoningContent(t *testing.T) {
	block := mkBlock(mkInvoke("exec_command", mkParam("cmd", "pwd")))
	rcFirst := "思考：" + block // 思维链里出现标记 → 绝不还原
	rcSecond := " 继续想"

	raw := markupRunStream(
		markupChunk(map[string]any{"role": "assistant", "reasoning_content": rcFirst}, ""),
		markupChunk(map[string]any{"reasoning_content": rcSecond, "content": block + "尾巴"}, ""),
		markupChunk(map[string]any{}, "stop"),
	)
	body := markupRunBody(t, raw, map[string]bool{"exec_command": true})
	frames := framesFromBody(t, body)

	var rc strings.Builder
	for _, fr := range frames {
		chs, _ := fr["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		if v, ok := d["reasoning_content"].(string); ok {
			rc.WriteString(v)
		}
	}
	if got, want := rc.String(), rcFirst+rcSecond; got != want {
		t.Errorf("reasoning_content 必须原样透出\nwant %q\ngot  %q", want, got)
	}

	p := markupScan(t, frames)
	if len(p.toolFrameAt) != 1 {
		t.Fatalf("思维链里的标记不得被还原：应只还原 content 里的 1 个调用，实际 %d", len(p.toolFrameAt))
	}
	if strings.Contains(p.content.String(), markupPipe) {
		t.Errorf("content 里的标记应被摘掉，实际 %q", p.content.String())
	}
	if !strings.Contains(p.content.String(), "尾巴") {
		t.Errorf("content 标记之外的正文被吞了: %q", p.content.String())
	}
	if strings.Contains(rc.String(), "tool_calls") {
		t.Errorf("reasoning_content 被改写了: %q", rc.String())
	}
}

// 用例④：不变量 —— 流尾 Flush 生效：未闭合的块（以及被增量边界截断的开始标记前缀）
// 必须在流尾原文写回，且不得被当成工具调用。
func TestStreamMarkupRepairFlushesTail(t *testing.T) {
	// (a) 未闭合的块：整段原文（含开始标记）只能靠流尾 Flush 交还。
	unclosed := markupOpen + "\n" + mkInvoke("exec_command", mkParam("cmd", "pwd"))
	raw := markupRunStream(
		markupChunk(map[string]any{"content": "开头。\n" + unclosed}, ""),
		markupChunk(map[string]any{}, "stop"),
	)
	p := markupScan(t, framesFromBody(t, markupRunBody(t, raw, map[string]bool{"exec_command": true})))
	if len(p.toolFrameAt) != 0 {
		t.Errorf("未闭合的块不得还原成调用，实际 %d 个调用帧", len(p.toolFrameAt))
	}
	if got, want := p.content.String(), "开头。\n"+unclosed; got != want {
		t.Errorf("流尾 Flush 未把未闭合块原文写出\nwant %q\ngot  %q", want, got)
	}

	// (b) 增量边界截断在开始标记中间：前缀同样由 Flush 交还，一个字节都不许丢。
	// 切点必须落在 rune 边界上（'<' + 两个全角竖线 = 7 字节）：真实上游是在 SSE 帧边界
	// 切分，帧内 JSON 恒为合法 UTF-8；在 rune 中间切会让 JSON 层把残字节换成 U+FFFD，
	// 那不是被测行为。
	prefix := markupOpen[:7]
	raw2 := markupRunStream(
		markupChunk(map[string]any{"content": "嗯" + prefix}, ""),
		markupChunk(map[string]any{}, "stop"),
	)
	p2 := markupScan(t, framesFromBody(t, markupRunBody(t, raw2, map[string]bool{"exec_command": true})))
	if got, want := p2.content.String(), "嗯"+prefix; got != want {
		t.Errorf("被截断的开始标记前缀必须原样交还\nwant %q\ngot  %q", want, got)
	}
	if len(p2.toolFrameAt) != 0 {
		t.Errorf("截断前缀不得还原成调用")
	}
}

// 用例⑤：不变量 4 —— Feed 返回空正文且没有调用时删掉 content 键：
// 该帧退化成 role 类空帧（非空内容帧不出现），且后续片能正常还原调用。
func TestStreamMarkupRepairDropsEmptyContentKey(t *testing.T) {
	raw := markupRunStream(
		// 整段正文都在「可能是标记开始」的缓冲里 → Feed 返回 "" 且无调用
		markupChunk(map[string]any{"role": "assistant", "content": markupOpen + "\n"}, ""),
		markupChunk(map[string]any{"content": mkInvoke("exec_command", mkParam("cmd", "pwd")) + "\n" + markupClose}, ""),
		markupChunk(map[string]any{}, "stop"),
	)
	frames := framesFromBody(t, markupRunBody(t, raw, map[string]bool{"exec_command": true}))
	p := markupScan(t, frames)

	if p.emptyContentFrames != 0 {
		t.Errorf("不得产生空内容帧（\"content\":\"\"），实际 %d 个", p.emptyContentFrames)
	}
	if p.content.String() != "" {
		t.Errorf("正文整段都在标记里，透出应为空，实际 %q", p.content.String())
	}
	if len(p.toolFrameAt) != 1 {
		t.Fatalf("应还原 1 个调用，实际 %d", len(p.toolFrameAt))
	}
	d, _ := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, has := d["content"]; has {
		t.Errorf(`content 键应被删除，实得 %#v`, d)
	}
	if d["role"] != "assistant" {
		t.Errorf("同帧的 role 应保留，实得 %#v", d)
	}
}
