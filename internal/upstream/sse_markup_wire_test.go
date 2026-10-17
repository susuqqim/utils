// sse_markup_wire_test.go 覆盖「流式路径把正文里的模型原生工具调用标记还原成
// delta.tool_calls」这条接线（sse.go 的 WithMarkupRepair / repairFrame / emitChunk /
// 流尾收尾），解析器本身见 dsml.go / dsml_test.go。
//
// 本文件守四条不变量（与任务书一一对应，每条都有对应用例）：
//  1. 含标记的流式响应被还原成 delta.tool_calls，且正文里不残留标记；
//  2. 客户端未声明 tools（Enabled() == false）→ 整条流零改写，输出与不接该功能时
//     **逐字节一致**，标记原样透出；
//  3. 流尾 Flush 生效：未判定的尾部缓冲（未闭合的块 / 被增量边界截断的开始标记前缀）
//     在流尾原文写回，且不被当成工具调用；
//  4. 一旦本流还原出工具调用，finish_reason 必为 tool_calls、只出现一次、且排在
//     调用帧之后——收尾挂在数据帧上、上游压根不给收尾帧两种形态都要成立。
//
// 另加两条守护：只碰 delta.content（reasoning_content 一字不动、不参与修复）；
// 工具名不在名单里时整块原文透出（绝不吞字节）。
//
// 夹具用包内常量（markupOpen / markupPipe …）与 dsml_test.go 的 mkBlock/mkInvoke/
// mkParam 拼装，不在源码里写死全角分隔符字面量。
package upstream

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- 夹具与解析 -------------------------------------------------------------

// wireChunk 拼一个「单 choice（index=0）」的上游 chunk；finish 为空则不带
// finish_reason 键（与真实上游一致：中间帧压根没有这个键）。
func wireChunk(delta map[string]any, finish string) map[string]any {
	c := map[string]any{"index": float64(0), "delta": delta}
	if finish != "" {
		c["finish_reason"] = finish
	}
	return map[string]any{
		"id":      "resp-wire",
		"object":  "chat.completion.chunk",
		"created": float64(1),
		"model":   "global:deepseek-v4.1-flash",
		"choices": []any{c},
	}
}

// wireUpstream 把若干上游 chunk 拼成 SSE 文本（data: 行 + 空行），末尾补 [DONE]。
func wireUpstream(frames ...map[string]any) string {
	var sb strings.Builder
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			panic(err) // 夹具自身的问题，不是被测行为
		}
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// wireRun 挂上修复器跑一遍流，返回响应体、修复器与 [DONE] 出现次数。
func wireRun(t *testing.T, raw string, allowed map[string]bool) (string, *MarkupRepair, int) {
	t.Helper()
	repair := NewMarkupRepair(allowed, false)
	rec := httptest.NewRecorder()
	if err := StreamContextOpt(context.Background(), rec, strings.NewReader(raw), false,
		WithMarkupRepair(repair)); err != nil {
		t.Fatalf("StreamContextOpt: %v", err)
	}
	body := rec.Body.String()
	return body, repair, strings.Count(body, "data: [DONE]")
}

// wireView 是网关写出帧的扁平视图：第几帧有正文 / 有调用 / 有收尾。
type wireView struct {
	content   strings.Builder
	callAt    []int
	finishAt  []int
	finishVal []string
}

func wireScan(t *testing.T, frames []map[string]any) wireView {
	t.Helper()
	var v wireView
	for i, fr := range frames {
		chs, _ := fr["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if s, ok := d["content"].(string); ok {
				v.content.WriteString(s)
			}
			if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
				v.callAt = append(v.callAt, i)
			}
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			v.finishAt = append(v.finishAt, i)
			v.finishVal = append(v.finishVal, fr)
		}
	}
	return v
}

// wireAssertSingleToolFinish 断言不变量 4：收尾恰好一个、值为 tool_calls、
// 且排在全部调用帧之后。
func wireAssertSingleToolFinish(t *testing.T, v wireView) {
	t.Helper()
	if len(v.finishAt) != 1 {
		t.Fatalf("finish_reason 应恰好一次，实际 %d 次（值 %v，帧 %v）", len(v.finishAt), v.finishVal, v.finishAt)
	}
	if v.finishVal[0] != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", v.finishVal[0])
	}
	if len(v.callAt) == 0 {
		t.Fatal("没有任何调用帧")
	}
	if last := v.callAt[len(v.callAt)-1]; v.finishAt[0] <= last {
		t.Fatalf("收尾帧在 %d，必须排在最后一个调用帧 %d 之后", v.finishAt[0], last)
	}
}

// --- ① 还原成 delta.tool_calls，正文不残留标记 -------------------------------

// TestSSEMarkupWireEndToEnd 上游把工具调用吐成正文标记（且标记被增量边界切成两片）：
// 网关必须还原成 delta.tool_calls、摘掉标记、并按规范收尾。
func TestSSEMarkupWireEndToEnd(t *testing.T) {
	prose := "先读一遍控制器，把接口清单核实准确。\n\n"
	block := mkBlock(
		mkInvoke("exec_command", mkParam("cmd", "ls -la")),
		mkInvoke("exec_command", mkParam("cmd", "pwd")),
	)
	// 刻意切在开始标记内部（真实上游按 SSE 帧边界切分，帧内 JSON 恒为合法 UTF-8）：
	// 跨帧缓冲必须能把两片拼回来。
	cut := len(markupOpen) - 2
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant"}, ""),
		wireChunk(map[string]any{"content": prose + block[:cut]}, ""),
		wireChunk(map[string]any{"content": block[cut:] + "尾巴"}, ""),
		wireChunk(map[string]any{}, "stop"),
	)
	body, repair, done := wireRun(t, raw, map[string]bool{"exec_command": true})
	if done != 1 {
		t.Fatalf("[DONE] 应恰好一个，实际 %d", done)
	}
	if strings.Contains(body, markupPipe) {
		t.Fatalf("透出内容里仍残留标记: %s", body)
	}
	if repair.Converted() != 2 || repair.Rejected() != 0 {
		t.Fatalf("Converted/Rejected=%d/%d want 2/0", repair.Converted(), repair.Rejected())
	}

	frames := framesFromBody(t, body)
	v := wireScan(t, frames)
	if got, want := v.content.String(), prose+"尾巴"; got != want {
		t.Fatalf("正文应只保留标记之外的部分\nwant %q\ngot  %q", want, got)
	}
	if len(v.callAt) != 2 {
		t.Fatalf("应还原 2 个 tool_call 帧，实际 %d（body=%s）", len(v.callAt), body)
	}
	seenIdx := map[float64]bool{}
	for n, fi := range v.callAt {
		c := frames[fi]["choices"].([]any)[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		if len(tcs) != 1 {
			t.Fatalf("帧 %d 应恰好带 1 个调用，实际 %d", fi, len(tcs))
		}
		tc, _ := tcs[0].(map[string]any)
		if tc["type"] != "function" {
			t.Fatalf("第 %d 个调用 type=%v want function", n, tc["type"])
		}
		if id, _ := tc["id"].(string); id == "" {
			t.Fatalf("第 %d 个调用缺 id: %#v", n, tc)
		}
		idx, ok := tc["index"].(float64)
		if !ok {
			t.Fatalf("第 %d 个调用 index 不是数值: %#v", n, tc["index"])
		}
		if seenIdx[idx] {
			t.Fatalf("两个调用复用了同一个 index=%v（会被客户端累加器揉成一个）", idx)
		}
		seenIdx[idx] = true
		fn, _ := tc["function"].(map[string]any)
		if fn == nil || fn["name"] != "exec_command" {
			t.Fatalf("第 %d 个调用函数名错误: %#v", n, tc)
		}
		argsJSON, _ := fn["arguments"].(string)
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			t.Fatalf("第 %d 个调用 arguments 不是合法 JSON: %q", n, argsJSON)
		}
		if want := []string{"ls -la", "pwd"}[n]; args["cmd"] != want {
			t.Fatalf("第 %d 个调用 cmd=%#v want %q", n, args["cmd"], want)
		}
	}
	wireAssertSingleToolFinish(t, v)
}

// --- ② 未声明 tools → 逐字节一致 --------------------------------------------

// TestSSEMarkupWireDisabledByteIdentical 客户端没声明 tools：三种「不启用」形态
// （不挂选项 / WithMarkupRepair(nil) / 只挂旁路观察者）的输出必须与不带该功能时
// 逐字节一致，标记原样透出、绝不产出 tool_calls。
func TestSSEMarkupWireDisabledByteIdentical(t *testing.T) {
	content := "先看一眼仓库结构。\n\n" +
		mkBlock(mkInvoke("exec_command", mkParam("cmd", "git status --short"))) +
		"\n结尾 <" // 末尾的 "<" 是「可能是标记起点」的前缀：启用时会被缓冲，未启用时必须原样透出
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant"}, ""),
		wireChunk(map[string]any{"content": content}, ""),
		wireChunk(map[string]any{}, "stop"),
	)
	ctx := context.Background()

	base := httptest.NewRecorder()
	if err := Stream(base, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	want := base.Body.String()
	if !strings.Contains(want, markupPipe) {
		t.Fatal("基线里本应带着标记（上游原样透传）")
	}

	cases := map[string][]StreamOption{
		"WithMarkupRepair(nil)": {WithMarkupRepair(nil)},
		// 客户端没声明过 tools → NewMarkupRepair(nil,false).Enabled() == false
		"未启用的修复器": {WithMarkupRepair(NewMarkupRepair(nil, false))},
		// 真实 handler 形态：没声明 tools 时只挂旁路观察者，压根不传 WithMarkupRepair
		"只挂旁路观察者": {WithErrorFrameObserver(func(string) {})},
	}
	for name, opts := range cases {
		rec := httptest.NewRecorder()
		if err := StreamContextOpt(ctx, rec, strings.NewReader(raw), false, opts...); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := rec.Body.String()
		if got != want {
			t.Fatalf("%s: 输出被改动（必须逐字节一致）\nwant %q\ngot  %q", name, want, got)
		}
		if strings.Contains(got, "tool_calls") {
			t.Fatalf("%s: 未启用时不得产出 tool_calls: %q", name, got)
		}
	}

	// 启用但工具名不在名单里（块内夹带正文 / 名字不合规）→ 同样原文透出，一个字节不吞。
	other := httptest.NewRecorder()
	if err := StreamContextOpt(ctx, other, strings.NewReader(raw), false,
		WithMarkupRepair(NewMarkupRepair(map[string]bool{"exec_command": true}, false))); err != nil {
		t.Fatal(err)
	}
	// content 里只有 exec_command 一个调用（是名单内的），这一路应发生修复。
	if strings.Contains(other.Body.String(), markupPipe) {
		t.Fatalf("名单命中时应修复并摘掉标记: %q", other.Body.String())
	}
}

// TestSSEMarkupWireUnknownToolNameVerbatim 标记块里的工具名不在客户端名单里
// （严格判定不过）→ 整块原文透出、不产出调用、finish_reason 保持上游原值。
func TestSSEMarkupWireUnknownToolNameVerbatim(t *testing.T) {
	block := mkBlock(mkInvoke("not_declared_tool", mkParam("x", "1")))
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant"}, ""),
		wireChunk(map[string]any{"content": "说明\n" + block}, ""),
		wireChunk(map[string]any{}, "stop"),
	)
	body, repair, _ := wireRun(t, raw, map[string]bool{"exec_command": true})
	if repair.Seen() != 1 || repair.Converted() != 0 || repair.Rejected() != 1 {
		t.Fatalf("Seen/Converted/Rejected=%d/%d/%d want 1/0/1", repair.Seen(), repair.Converted(), repair.Rejected())
	}
	v := wireScan(t, framesFromBody(t, body))
	if len(v.callAt) != 0 {
		t.Fatalf("判定不通过时不得产出调用帧，实际 %d", len(v.callAt))
	}
	if got, want := v.content.String(), "说明\n"+block; got != want {
		t.Fatalf("判定不通过时必须原文透出\nwant %q\ngot  %q", want, got)
	}
	if len(v.finishAt) != 1 || v.finishVal[0] != "stop" {
		t.Fatalf("finish_reason 应保持上游原值 stop，实际 %v@%v", v.finishVal, v.finishAt)
	}
}

// --- ③ 流尾 Flush 生效 ------------------------------------------------------

// TestSSEMarkupWireFlushesTail 未闭合的块、被增量边界截断的开始标记前缀，都在流尾
// 原文写回，且不被当成工具调用。
func TestSSEMarkupWireFlushesTail(t *testing.T) {
	t.Run("未闭合的块", func(t *testing.T) {
		unclosed := markupOpen + "\n" + mkInvoke("exec_command", mkParam("cmd", "pwd"))
		raw := wireUpstream(
			wireChunk(map[string]any{"content": "说明\n" + unclosed}, ""),
			wireChunk(map[string]any{}, "stop"),
		)
		body, repair, _ := wireRun(t, raw, map[string]bool{"exec_command": true})
		if repair.Converted() != 0 {
			t.Fatalf("未闭合块不得转换，实际 %d", repair.Converted())
		}
		v := wireScan(t, framesFromBody(t, body))
		if len(v.callAt) != 0 {
			t.Fatalf("不得产出 tool_call，实际 %d", len(v.callAt))
		}
		if got, want := v.content.String(), "说明\n"+unclosed; got != want {
			t.Fatalf("未闭合块应由流尾 Flush 原样交还\nwant %q\ngot  %q", want, got)
		}
		if len(v.finishAt) != 1 || v.finishVal[0] != "stop" {
			t.Fatalf("没有还原出调用时 finish_reason 不应被改写，实际 %v@%v", v.finishVal, v.finishAt)
		}
	})
	t.Run("截断的开始标记前缀", func(t *testing.T) {
		// 切点落在 rune 边界：'<' + 两个全角竖线（各 3 字节）+ 'D' = 7 字节。
		prefix := markupOpen[:7]
		raw := wireUpstream(
			wireChunk(map[string]any{"content": "嗯" + prefix}, ""),
			wireChunk(map[string]any{}, "stop"),
		)
		body, _, _ := wireRun(t, raw, map[string]bool{"exec_command": true})
		v := wireScan(t, framesFromBody(t, body))
		if len(v.callAt) != 0 {
			t.Fatalf("截断前缀不得还原成调用")
		}
		if got, want := v.content.String(), "嗯"+prefix; got != want {
			t.Fatalf("截断前缀应由流尾 Flush 原样交还\nwant %q\ngot  %q", want, got)
		}
	})
}

// --- ④ finish_reason 恰好一次且为 tool_calls --------------------------------

// TestSSEMarkupWireFinishOnDataFrame 上游把 finish_reason 贴在**带标记的数据帧**上
// （且后面还跟一个空的收尾帧）：收尾必须被挪到调用帧之后，且全流只有一个。
func TestSSEMarkupWireFinishOnDataFrame(t *testing.T) {
	block := mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls")))
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant"}, ""),
		wireChunk(map[string]any{"content": "看目录\n" + block}, "stop"),
		wireChunk(map[string]any{}, "stop"),
	)
	body, _, _ := wireRun(t, raw, map[string]bool{"exec_command": true})
	v := wireScan(t, framesFromBody(t, body))
	if got, want := v.content.String(), "看目录\n"; got != want {
		t.Fatalf("正文=%q want %q", got, want)
	}
	if len(v.callAt) != 1 {
		t.Fatalf("应还原 1 个调用帧，实际 %d（body=%s）", len(v.callAt), body)
	}
	wireAssertSingleToolFinish(t, v)
}

// TestSSEMarkupWireFinishOnlyAtTail 上游压根不给收尾帧（数据帧带标记后直接 [DONE]）：
// 网关必须在流尾兜底补一帧 finish_reason: tool_calls，且只有一个。
func TestSSEMarkupWireFinishOnlyAtTail(t *testing.T) {
	block := mkBlock(mkInvoke("exec_command", mkParam("cmd", "ls")))
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant"}, ""),
		wireChunk(map[string]any{"content": "看目录\n" + block}, "stop"),
	)
	// 上面这帧就把 finish_reason 摘走了；上游再没有别的帧 → 只能靠流尾兜底。
	body, repair, done := wireRun(t, raw, map[string]bool{"exec_command": true})
	if done != 1 {
		t.Fatalf("[DONE] 应恰好一个，实际 %d", done)
	}
	if repair.Converted() != 1 {
		t.Fatalf("Converted=%d want 1", repair.Converted())
	}
	v := wireScan(t, framesFromBody(t, body))
	if len(v.callAt) != 1 {
		t.Fatalf("应还原 1 个调用帧，实际 %d（body=%s）", len(v.callAt), body)
	}
	wireAssertSingleToolFinish(t, v)
}

// --- 守护：只碰 content，reasoning_content 一字不动 --------------------------

// TestSSEMarkupWireReasoningUntouched 思维链里出现标记绝不还原；正文里的标记照常
// 还原；两段 reasoning_content 必须原样、按序透出。
func TestSSEMarkupWireReasoningUntouched(t *testing.T) {
	block := mkBlock(mkInvoke("exec_command", mkParam("cmd", "pwd")))
	rc1 := "思考：" + block
	rc2 := " 继续想"
	raw := wireUpstream(
		wireChunk(map[string]any{"role": "assistant", "reasoning_content": rc1}, ""),
		wireChunk(map[string]any{"reasoning_content": rc2, "content": block + "尾巴"}, ""),
		wireChunk(map[string]any{}, "stop"),
	)
	body, _, _ := wireRun(t, raw, map[string]bool{"exec_command": true})
	frames := framesFromBody(t, body)

	var rc strings.Builder
	gotRC := 0
	for _, fr := range frames {
		chs, _ := fr["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		c, _ := chs[0].(map[string]any)
		d, _ := c["delta"].(map[string]any)
		if v, ok := d["reasoning_content"].(string); ok {
			rc.WriteString(v)
			gotRC++
		}
	}
	if gotRC != 2 {
		t.Fatalf("reasoning_content 应原样出现在 2 帧里，实际 %d 帧", gotRC)
	}
	if got, want := rc.String(), rc1+rc2; got != want {
		t.Fatalf("reasoning_content 被改写了\nwant %q\ngot  %q", want, got)
	}
	if !strings.Contains(rc.String(), markupPipe) {
		t.Fatal("思维链里的标记应原样保留（不参与修复）")
	}

	v := wireScan(t, frames)
	if len(v.callAt) != 1 {
		t.Fatalf("思维链里的标记不得被还原：应只还原 content 里的 1 个调用，实际 %d", len(v.callAt))
	}
	if got, want := v.content.String(), "尾巴"; got != want {
		t.Fatalf("正文=%q want %q", got, want)
	}
	wireAssertSingleToolFinish(t, v)
}
