// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"workbuddy2api/internal/gosafe"
)

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// isErrorEventLine 判断是否为 `event: error`（大小写不敏感、容忍冒号后空白）。
// 只有这一种事件名被透传：它携带"本帧是错误"的语义，客户端据此选择错误分支。
func isErrorEventLine(line string) bool {
	const prefix = "event:"
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line[len(prefix):]), "error")
}

// SSEDataPayload 提取一行 SSE 的 data 负载，容忍 "data:" 后带或不带一个空格。
// 部分上游/中间层发 `data:{...}`；旧实现硬编码 "data: " 会把这类帧当普通行原样
// 透传（跳过 reasoning 别名、tool_call 回填、cache 别名等全部归一化），非流式则
// 判成空流回 502。
func SSEDataPayload(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(line[len("data:"):], " "), true
}

func Aggregate(r io.Reader) (map[string]any, error) {
	return AggregateContext(context.Background(), r)
}

// AggregateContext 读取完整 SSE 流，支持 context 监听客户端中断。
func AggregateContext(ctx context.Context, r io.Reader) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	br := bufio.NewReaderSize(r, 64*1024)

	// 按 choice.index 分桶聚合：旧实现把所有 choice 的 delta 拼进同一个 content，
	// n>1 时会把多条回答揉成一条、且只回 choice 0。
	type choiceAcc struct {
		content      strings.Builder
		reasoning    strings.Builder
		toolCalls    map[int]map[string]any
		toolOrder    []int
		functionCall map[string]any
		finish       string
	}
	var (
		id, model, systemFP, serviceTier string
		created                          float64
		usage                            map[string]any
		choiceMap                        = map[int]*choiceAcc{}
		choiceOrder                      []int
		validEvents                      int
		sawDone                          bool
		anyFinish                        bool
		totalBytes                       int
	)
	const maxAggregateBytes = 64 << 20 // 聚合上限：防异常/恶意上游无限增长内存

	get := func(idx int) *choiceAcc {
		c := choiceMap[idx]
		if c == nil {
			c = &choiceAcc{toolCalls: map[int]map[string]any{}}
			choiceMap[idx] = c
			choiceOrder = append(choiceOrder, idx)
		}
		return c
	}

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if payload, ok := SSEDataPayload(line); ok {
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				// 上游错误帧（HTTP 200 之后才出现的限流/审核等）：非流式没有可回传的
				// 流式错误通道，必须直接报错；旧实现会把它当"成功但空回答"。
				if em, hasErr := chunk["error"]; hasErr && em != nil {
					b, _ := json.Marshal(em)
					return nil, fmt.Errorf("upstream error frame: %s", b)
				}
				validEvents++
				if v, ok := chunk["id"].(string); ok && id == "" {
					id = v
				}
				if v, ok := chunk["model"].(string); ok && model == "" {
					model = v
				}
				if v, ok := chunk["created"].(float64); ok && created == 0 {
					created = v
				}
				if v, ok := chunk["system_fingerprint"].(string); ok && systemFP == "" {
					systemFP = v
				}
				if v, ok := chunk["service_tier"].(string); ok && serviceTier == "" {
					serviceTier = v
				}
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				if chs, ok := chunk["choices"].([]any); ok {
					for _, ci := range chs {
						c, _ := ci.(map[string]any)
						if c == nil {
							continue
						}
						idx := 0
						if v, ok := c["index"].(float64); ok {
							idx = int(v)
						}
						acc := get(idx)
						if fr, ok := c["finish_reason"].(string); ok && fr != "" {
							acc.finish = fr
							anyFinish = true
						}
						if delta, ok := c["delta"].(map[string]any); ok {
							if txt, ok := delta["content"].(string); ok {
								acc.content.WriteString(txt)
								totalBytes += len(txt)
							}
							if rc, ok := delta["reasoning_content"].(string); ok {
								acc.reasoning.WriteString(rc)
							} else if r2, ok := delta["reasoning"].(string); ok {
								acc.reasoning.WriteString(r2)
							} else if t, ok := delta["thought"].(string); ok {
								acc.reasoning.WriteString(t)
							}
							if tcs, ok := delta["tool_calls"].([]any); ok {
								for _, tc := range tcs {
									call, ok := tc.(map[string]any)
									if !ok {
										continue
									}
									tidx := 0
									if v, ok := call["index"].(float64); ok {
										tidx = int(v)
									}
									merged, seen := acc.toolCalls[tidx]
									if !seen {
										merged = map[string]any{"index": tidx}
										acc.toolCalls[tidx] = merged
										acc.toolOrder = append(acc.toolOrder, tidx)
									}
									mergeToolCallDelta(merged, call)
								}
							}
							// legacy function_call：与 tool_calls 一样跨帧拼接（旧实现丢这一步）
							if fc, ok := delta["function_call"].(map[string]any); ok {
								if acc.functionCall == nil {
									acc.functionCall = map[string]any{}
								}
								mergeFunctionCallDelta(acc.functionCall, fc)
							}
						}
						// 有的上游把完整消息放在 message 里（非 delta）
						if msg, ok := c["message"].(map[string]any); ok {
							if acc.content.Len() == 0 {
								if txt, ok := msg["content"].(string); ok {
									acc.content.WriteString(txt)
									totalBytes += len(txt)
								}
							}
							if fc, ok := msg["function_call"].(map[string]any); ok && acc.functionCall == nil {
								acc.functionCall = map[string]any{}
								mergeFunctionCallDelta(acc.functionCall, fc)
							}
						}
					}
				}
				if totalBytes > maxAggregateBytes {
					return nil, fmt.Errorf("upstream stream exceeded aggregate limit (%d bytes)", maxAggregateBytes)
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, fmt.Errorf("upstream stream contained no valid data events")
	}
	if !sawDone && !anyFinish {
		// 既无 [DONE] 也无任何 finish_reason：流被截断。旧实现默认 stop，
		// 会把截断伪装成正常完成。
		return nil, fmt.Errorf("upstream stream ended without finish_reason or [DONE] (truncated)")
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	sortInts(choiceOrder)
	choices := make([]any, 0, len(choiceOrder))
	for _, idx := range choiceOrder {
		acc := choiceMap[idx]
		message := map[string]any{
			"role":    "assistant",
			"content": acc.content.String(),
		}
		if acc.reasoning.Len() > 0 {
			message["reasoning_content"] = acc.reasoning.String()
		}
		if len(acc.toolOrder) > 0 {
			sortInts(acc.toolOrder)
			calls := make([]map[string]any, 0, len(acc.toolOrder))
			for _, ti := range acc.toolOrder {
				calls = append(calls, acc.toolCalls[ti])
			}
			message["tool_calls"] = calls
		}
		if acc.functionCall != nil {
			message["function_call"] = acc.functionCall
		}
		fr := acc.finish
		if fr == "" {
			if len(acc.toolOrder) > 0 {
				fr = "tool_calls"
			} else if acc.functionCall != nil {
				fr = "function_call"
			} else {
				fr = "stop"
			}
		} else if fr == "stop" && len(acc.toolOrder) > 0 {
			fr = "tool_calls"
		}
		choices = append(choices, map[string]any{
			"index":         idx,
			"message":       message,
			"finish_reason": fr,
		})
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": choices,
	}
	if systemFP != "" {
		resp["system_fingerprint"] = systemFP
	}
	if serviceTier != "" {
		resp["service_tier"] = serviceTier
	}
	if usage != nil {
		resp["usage"] = normalizeUsageCacheAliases(usage)
	}
	return resp, nil
}

// mergeFunctionCallDelta 合并 legacy function_call 的 name/arguments 增量。
func mergeFunctionCallDelta(merged, delta map[string]any) {
	if v, ok := delta["name"].(string); ok && v != "" {
		merged["name"] = v
	}
	if v, ok := delta["arguments"].(string); ok && v != "" {
		if prev, _ := merged["arguments"].(string); prev != "" {
			merged["arguments"] = prev + v
		} else {
			merged["arguments"] = v
		}
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

type ToolCallMeta struct {
	Name string
	ID   string
	Type string
	// Announced 该 (choice, tool_call) 的元数据是否**已经发过**。
	// OpenAI 流式规范：`id`/`type`/`function.name` 只在**首帧**出现，后续增量帧只带
	// `index` + `function.arguments`。旧实现给每个续帧都补回元数据，导致按"带 id 即新调用"
	// 解析的客户端（实测 Codex）把一次工具调用拆成几百个碎片（实测 327 帧 / 1~11 字符每帧）。
	Announced bool
}

// backfillToolCallMeta 维护跨帧的 tool_calls name/id/type 缓存并回填。
// 缓存维度 [choice.index][tool_call.index]：n>1 时不同 choice 的同号 tool_call
// 必须各自独立，否则后到的会覆盖先到的 name/id/type（跨 choice 串台）。
func backfillToolCallMeta(obj map[string]any, metaCache map[int]map[int]ToolCallMeta, repeat bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		cidx := 0
		if v, ok := c["index"].(float64); ok {
			cidx = int(v)
		}
		perChoice := metaCache[cidx]
		if perChoice == nil {
			perChoice = map[int]ToolCallMeta{}
			metaCache[cidx] = perChoice
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}

			meta := perChoice[idx]

			fn, _ := tc["function"].(map[string]any)
			if fn != nil {
				if v, ok := fn["name"].(string); ok && v != "" {
					meta.Name = v
				}
			}
			if v, ok := tc["id"].(string); ok && v != "" {
				meta.ID = v
			}
			if v, ok := tc["type"].(string); ok && v != "" {
				meta.Type = v
			}

			// 续帧规范化（非 repeat 模式）：**删掉**续帧上的 id/type/function.name，
			// 只留 index + function.arguments。上游实测会每帧都带 type（部分还带 name/id），
			// 而规范只在首帧给 —— 多余元数据会让按"带元数据即新调用"解析的客户端
			// （实测 Codex"工具调用被逐字符拆碎"）崩掉。
			if !repeat && meta.Announced {
				delete(tc, "id")
				delete(tc, "type")
				if f, ok := tc["function"].(map[string]any); ok {
					delete(f, "name")
				}
				continue
			}
			// 默认只在该 (choice, tool_call) 的**首帧**补元数据（OpenAI 规范）。续帧一律只留
			// index + arguments —— 重复补 id/name 会把严格客户端（Codex）拆成几百个碎片。
			// repeat=true 时恢复旧行为（每个续帧都补），供需要重复元数据的客户端使用。
			if repeat || !meta.Announced {
				wrote := false
				if meta.Name != "" {
					if fn == nil {
						fn = map[string]any{}
						tc["function"] = fn
					}
					if n, ok := fn["name"].(string); !ok || n == "" {
						fn["name"] = meta.Name
						wrote = true
					} else {
						wrote = true // 上游本帧就带了 name → 也算已宣告
					}
				}
				if meta.ID != "" {
					if id, ok := tc["id"].(string); !ok || id == "" {
						tc["id"] = meta.ID
						wrote = true
					} else {
						wrote = true
					}
				}
				if meta.Type != "" {
					if t, ok := tc["type"].(string); !ok || t == "" {
						tc["type"] = meta.Type
						wrote = true
					} else {
						wrote = true
					}
				} else if meta.Name != "" {
					if t, ok := tc["type"].(string); !ok || t == "" {
						tc["type"] = "function"
						wrote = true
					}
				}
				if wrote {
					meta.Announced = true
				}
			}
			// 只在"确实学到东西"时才建/写缓存条目：否则任意未知 index 都会留下空条目，
			// 既污染缓存也让"某 index 从未出现过"这一信号失真。
			if meta.Name != "" || meta.ID != "" || meta.Type != "" || meta.Announced {
				perChoice[idx] = meta
			}
		}
	}
}

// remappedFinish 把"重复贴在数据帧上的 finish_reason"移到真正的收尾帧。
//
// 背景（2026-10-03，用户实测 Codex 报"工具调用被逐字符拆碎、无法发送代码"）：
// 部分上游在每个增量帧都重复 `finish_reason`（实测一次 write_file 的 331 个工具帧
// **全部**带 `finish_reason:"tool_calls"`），而 OpenAI 规范要求只有**末帧**非 null。
// 客户端见到"帧带 finish_reason"就认为该次调用已结束 → 每个 1~6 字符的分片都被当成
// 一次独立工具调用（参数逐字符碎裂）。这里把数据帧上的值摘进 pending，仅在收尾帧发出。
func remappedFinish(frame map[string]any, pending map[int]string) {
	choices, _ := frame["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		cidx := 0
		if v, ok := c["index"].(float64); ok {
			cidx = int(v)
		}
		fr, _ := c["finish_reason"].(string)
		d, _ := c["delta"].(map[string]any)
		if deltaHasSubstance(d) {
			// 数据帧：finish_reason 一律摘掉（记住值，收尾帧再发）
			if fr != "" {
				pending[cidx] = fr
			}
			c["finish_reason"] = nil
			continue
		}
		// 非数据帧（空 delta / 仅 usage / 仅 role）：这才是规范的收尾帧
		if fr == "" {
			fr = pending[cidx]
		}
		if fr != "" {
			c["finish_reason"] = fr
			delete(pending, cidx)
		} else {
			c["finish_reason"] = nil
		}
	}
}

// deltaHasSubstance 报告 delta 是否携带实质内容（role/usage 这类不算）。
// 只有 role 的帧、空帧都视为"收尾帧候选"。
func deltaHasSubstance(d map[string]any) bool {
	if d == nil {
		return false
	}
	for k, v := range d {
		switch k {
		case "role", "usage":
			continue
		case "content", "reasoning_content":
			if s, ok := v.(string); ok && s == "" {
				continue
			}
		case "tool_calls":
			if a, ok := v.([]any); ok && len(a) == 0 {
				continue
			}
		case "function_call":
			if m, ok := v.(map[string]any); ok && len(m) == 0 {
				continue
			}
		}
		return true
	}
	return false
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any, toolCallsByChoice map[int]bool) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			cidx := 0
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
				if v, ok := idx.(float64); ok {
					cidx = int(v)
				}
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				// 统一多别名 reasoning
				var rc string
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					rc = v
				} else if v, ok := d["reasoning"].(string); ok && v != "" {
					rc = v
				} else if v, ok := d["thought"].(string); ok && v != "" {
					rc = v
				}
				if rc != "" {
					delta["reasoning_content"] = rc
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				// 白名单只认已知键时，logprobs/audio 这类合法输出会被静默丢弃
				// （客户端显式请求 logprobs 或音频输出时表现为"什么都没返回"）。
				if v, ok := d["logprobs"]; ok && v != nil {
					delta["logprobs"] = v
				}
				if v, ok := d["audio"]; ok && v != nil {
					delta["audio"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					// 续帧里的**空** name/id/type 一律删掉：规范只在首帧给元数据，
					// 而 `"name":""` / `"id":""` 这种空串会被严格客户端当成"又一个新调用"
					// （与"续帧重复带 id"同源的碎裂问题，见 backfillToolCallMeta 的注释）。
					for _, tci := range tcs {
						tc, _ := tci.(map[string]any)
						if tc == nil {
							continue
						}
						for _, k := range []string{"id", "type"} {
							if v, ok := tc[k].(string); ok && v == "" {
								delete(tc, k)
							}
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							if v, ok := fn["name"].(string); ok && v == "" {
								delete(fn, "name")
							}
						}
					}
					delta["tool_calls"] = tcs
					if toolCallsByChoice != nil {
						toolCallsByChoice[cidx] = true
					}
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta

			fr, hasFr := c["finish_reason"].(string)
			// 纠正只认"本 choice 自己的" tool_calls：共享标志会让 n>1 下纯文本的
			// choice 被改写成 tool_calls。
			choiceHasTools := toolCallsByChoice != nil && toolCallsByChoice[cidx]
			if hasFr && fr != "" {
				if (fr == "stop" || fr == "") && choiceHasTools {
					nc["finish_reason"] = "tool_calls"
				} else {
					nc["finish_reason"] = fr
				}
			} else {
				if choiceHasTools && hasFr && fr == "" {
					nc["finish_reason"] = "tool_calls"
				} else {
					nc["finish_reason"] = nil
				}
			}

			if v, ok := c["logprobs"]; ok && v != nil {
				nc["logprobs"] = v
			}
			if v, ok := c["audio"]; ok && v != nil {
				nc["audio"] = v
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if rawUsage, ok := obj["usage"]; ok {
		if u, ok := rawUsage.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(u)
		} else {
			out["usage"] = rawUsage
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamContext(context.Background(), w, r)
}

// StreamContextOpt 同 StreamContext，但显式指定 tool_call 元数据兼容开关
// （repeatMeta=true = 每个续帧都重复 id/name，见 Client.ToolCallMetaRepeat）。
func StreamContextOpt(ctx context.Context, w http.ResponseWriter, r io.Reader, repeatMeta bool, opts ...StreamOption) error {
	return streamContext(ctx, w, r, repeatMeta, applyStreamOptions(opts))
}

// StreamContext 带 context 的流式透传：监听下游客户端生命周期，
// 当下游主动断开连接（如点击“停止生成”或网络断开）时，立即返回 ctx.Err()，
// 促使外层立即 Close 上游连接，秒级掐断上游 token 生成以止血省积分。
func StreamContext(ctx context.Context, w http.ResponseWriter, r io.Reader) error {
	return streamContext(ctx, w, r, false, nil)
}

// streamContext 流式透传的实现体；repeatMeta 见 StreamContextOpt。
// applyStreamOptions 把选项列表折叠成一个配置（无选项时返回 nil，热路径零开销）。
func applyStreamOptions(opts []StreamOption) *streamOptions {
	if len(opts) == 0 {
		return nil
	}
	o := &streamOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(o)
		}
	}
	return o
}

// StreamOption 流式入口的可选行为开关。
// WithErrorFrameObserver 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站帧
// （把正文里的原生工具调用标记还原成 delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	onErrorFrame func(payload string)
	markup       *MarkupRepair
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
//
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，只执行结构化 function_call 的
// 客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次都没跑。挂上修复器后，
// 这类标记在透传前被还原成 delta.tool_calls。
//
// 传 nil、或传入未启用的修复器（客户端没声明过 tools）→ 与 Stream 行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

// WithErrorFrameObserver 注册"上游以 error 帧报错"的观察者：把该帧**原文**交给回调
// （帧仍照常透传给客户端），供调用方在流结束后按帧内容分类处置账号 —— 上游真实形态是
// 「HTTP 200 已开流 + 一帧 error」（6004 限流 / 内容拦截 / 审核），若不在流尾处置，
// 正在限流的号会被记成健康、粘性还会把会话钉死在它身上。
func WithErrorFrameObserver(fn func(payload string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}

func streamContext(ctx context.Context, w http.ResponseWriter, r io.Reader, repeatMeta bool, opts *streamOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	toolCallMeta := map[int]map[int]ToolCallMeta{}
	firstID := ""
	firstModel := ""
	toolCallsByChoice := map[int]bool{}
	// pendingFinish 见 remappedFinish：上游把 finish_reason 重复贴在数据帧上时，
	// 先摘下来、等真正的收尾帧再发（OpenAI 规范：只有末帧非 null）。
	pendingFinish := map[int]string{}

	// 标记修复（见 dsml.go）：把上游正文里的原生工具调用标记还原成 delta.tool_calls。
	// 未启用（客户端没声明 tools / 没挂修复器）时整条流零改写、逐帧零额外分配——
	// 与修复前逐字节一致（TestSSEMarkupWireDisabledByteIdentical 守这条不变量）。
	// 启用后的跨帧状态：
	//   - synthIdx：本流已还原的调用数（合成 index 从上游最大 index 之后接续）；
	//   - maxToolIdx：上游已用过的最大 tool_call index，避免合成 index 与真实调用撞车；
	//   - finishEmitted：哪些 choice 已经把收尾帧写出（流尾据此决定要不要兜底补一帧）。
	repair := (*MarkupRepair)(nil)
	if opts != nil {
		repair = opts.markup
	}
	repairEnabled := repair.Enabled()
	synthIdx := 0
	maxToolIdx := -1
	var finishEmitted map[int]bool // nil = 修复层未启用（零分配）

	// writeSSE 原样写出一帧（data: 行 + 空行）并 flush：透传帧与标记修复展开出的
	// 合成帧共用这一个出口，保证两条路径的写出字节形态完全一致。
	writeSSE := func(payload string) error {
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// writeRaw 原样写出一帧（绕过 normalizeFrame）。上游 error 帧（error-passthrough）
	// 与空流错误帧需保留 error 字段，不能被白名单剥掉，故经此写出。
	writeRaw := func(payload string) error {
		return writeSSE(payload)
	}

	// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被摘掉
	// 后的可见文本）+ N 个 delta.tool_calls 帧。没有命中标记时原样返回入参帧（单元素），
	// 与修复前逐字节一致。
	//
	// 只处理正文 content，reasoning_content 一个字都不动（思维链不参与修复）。
	//
	// 刻意**不**在这里补收尾帧：本帧若自带 finish_reason 就先摘下来（收尾必须排在调用帧
	// 之后，否则客户端在读到调用之前就认为回合结束了）；上游随后的收尾帧会被
	// normalizeFrame / promoteToolFinish 改写成 tool_calls，若上游压根不给收尾帧，流尾
	// 按需补一帧（见函数尾）。这样任何路径都只产出**一个** finish_reason。
	// 仅在 repair.Enabled() 时挂载，未启用时为 nil（逐帧零额外分配）。
	var repairFrame func(obj map[string]any) []map[string]any
	if repairEnabled {
		repairFrame = func(obj map[string]any) []map[string]any {
			chs, ok := obj["choices"].([]any)
			if !ok {
				return []map[string]any{obj}
			}
			type restoredCall struct {
				cidx int
				call MarkupCall
			}
			var got []restoredCall
			for _, ci := range chs {
				c, ok := ci.(map[string]any)
				if !ok {
					continue
				}
				d, ok := c["delta"].(map[string]any)
				if !ok {
					continue
				}
				txt, ok := d["content"].(string)
				if !ok || txt == "" {
					continue
				}
				emit, calls := repair.Feed(txt)
				if emit == "" {
					delete(d, "content")
				} else {
					d["content"] = emit
				}
				cidx := 0
				if v, ok := c["index"].(float64); ok {
					cidx = int(v)
				}
				for _, call := range calls {
					got = append(got, restoredCall{cidx: cidx, call: call})
				}
			}
			if len(got) == 0 {
				return []map[string]any{obj}
			}
			takeFinishReason(obj)
			out := make([]map[string]any, 0, 1+len(got))
			out = append(out, obj)
			for _, r := range got {
				out = append(out, markupCallFrame(obj, r.cidx, maxToolIdx+1+synthIdx, r.call))
				synthIdx++
			}
			return out
		}
	}

	// emitChunk 把一个 chunk 对象按规范白名单重建后写出并 flush，同时维护跨帧状态：
	// tool_call 元数据收敛（每 index 仅首片带 name/id/type）、消息 id 续传、上游重复贴在
	// 数据帧上的 finish_reason 归位（见 remappedFinish），以及「本流是否已写出收尾帧」。
	// fallback 是规范化失败时的兜底原文（与修复前「marshal 失败即原样透传」一致）。
	emitChunk := func(obj map[string]any, fallback string) error {
		backfillToolCallMeta(obj, toolCallMeta, repeatMeta)
		if firstID == "" {
			if v, ok := obj["id"].(string); ok && v != "" {
				firstID = v
			}
			if v, ok := obj["model"].(string); ok && v != "" {
				firstModel = v
			}
		} else if v, ok := obj["id"].(string); !ok || v == "" {
			obj["id"] = firstID
		}
		nf := normalizeFrame(obj, toolCallsByChoice)
		remappedFinish(nf, pendingFinish)
		if repairEnabled {
			// 修复层还原出工具调用的流：finish_reason 必须是 tool_calls。normalizeFrame
			// 已做过一次 stop→tool_calls，但 remappedFinish 把上游**重复贴在数据帧上**的
			// finish_reason 挪到收尾帧时会绕过那一次归一化，这里在最终写出前再兜一次。
			promoteToolFinish(nf, toolCallsByChoice)
			if frameFinishReason(nf) != "" {
				if finishEmitted == nil {
					finishEmitted = map[int]bool{}
				}
				noteFinishedChoices(nf, finishEmitted)
			}
		}
		raw, err := json.Marshal(nf)
		if err != nil {
			return writeSSE(fallback)
		}
		return writeSSE(string(raw))
	}

	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			// 上游错误帧透传（error-passthrough）：带 error 键的帧原样写出，不走
			// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
			// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
			// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
			if _, hasErr := obj["error"]; hasErr {
				if opts != nil && opts.onErrorFrame != nil {
					opts.onErrorFrame(payload) // 旁路观测：原文交回调，帧照常透传
				}
				if err := writeRaw(payload); err != nil {
					return 0, err
				}
				return 1, nil
			}
			// 标记修复：一帧可能展开成多帧（正文帧 + N 个 delta.tool_calls 帧）。
			// 修复器未启用时走单帧直传，与修复前逐字节一致（单帧路径与旧实现同构）。
			if repairEnabled {
				if m := maxToolCallIndex(obj); m > maxToolIdx {
					maxToolIdx = m
				}
				for _, ch := range repairFrame(obj) {
					if werr := emitChunk(ch, payload); werr != nil {
						return 0, werr
					}
				}
			} else if werr := emitChunk(obj, payload); werr != nil {
				return 0, werr
			}
			valid = 1
		} else if werr := writeSSE(payload); werr != nil {
			// 非 JSON 帧：原样透出，不计数（与修复前一致）。
			return 0, werr
		}
		return valid, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	var sawDone bool // 上游是否显式发了 [DONE]（未发 = 静默截断，见函数尾留痕）
	validFrames := 0
	var streamErr error

	lines := make(chan string)
	errs := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)

	// 读循环必须自带兜底：它经 bufio → chatStatsReader.Read → parseSSELine 解析**上游不可信
	// 字节**（json.Unmarshal 等），一旦 panic，goroutine 内的 panic 不会被 handler recover
	// 兜住 → 整个网关进程退出。转成一条流错误即可（读侧按 error 帧告知客户端）。
	go gosafe.DoErr("sse read loop", func(perr error) {
		select {
		case errs <- perr:
		default: // 消费方已退出（done 关闭）：不阻塞
		}
	}, func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				if line != "" {
					select {
					case lines <- line:
					case <-done:
						return
					}
				}
				select {
				case errs <- err:
				case <-done:
				}
				return
			}
			select {
			case lines <- line:
			case <-done:
				return
			}
		}
	})

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

readLoop:
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(10 * time.Second)
		select {
		case <-ctx.Done():
			// 客户端主动断开连接（如在前端点击“停止生成”）：秒级退出，促使上层立即切断上游连接以止血
			return ctx.Err()
		case line := <-lines:
			trimmed := strings.TrimRight(line, "\r\n")
			if payload, ok := SSEDataPayload(trimmed); ok {
				if payload == "[DONE]" {
					sawDone = true
					break readLoop
				}
				n, werr := writeFrame(payload)
				validFrames += n
				if werr != nil {
					return werr
				}
				continue
			}
			// 非 data 行只透传两类：SSE 注释（如上游的 ": keep-alive"）与 `event: error`
			// （错误事件名对客户端有语义）。`id:`/`retry:`/`event:` 其它值一律丢弃——
			// 它们不属于 Chat Completions 分片，原样下发会让部分 SDK 解析混乱。
			if strings.HasPrefix(trimmed, ":") || isErrorEventLine(trimmed) {
				if _, werr := io.WriteString(w, line); werr != nil {
					return werr
				}
				if fl != nil {
					fl.Flush()
				}
			}
		case err := <-errs:
			if err != io.EOF {
				streamErr = err
			}
			break readLoop
		case <-timer.C:
			if _, werr := io.WriteString(w, ": keep-alive\n\n"); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error"}}`)
	} else if streamErr != nil {
		// 中途读错误：下游只看到 [DONE] 会把截断当正常完成，这里补一个 error 帧。
		if msg, merr := json.Marshal(streamErr.Error()); merr == nil {
			_ = writeRaw(`{"error":{"message":` + string(msg) + `,"type":"upstream_error"}}`)
		}
	} else if !sawDone {
		// 有有效帧、无读错误，但上游**没发 [DONE]** 就 EOF：语义上属于"静默截断"。
		// 非流式聚合对同一形态判 truncation 并报错，而流式这里按"正常结束"兜底补 [DONE]
		// （客户端已收到全部帧，改判 502 会把大量正常请求变成失败——语义变更需产品决策）。
		// 但至少要留痕：否则运维完全看不到"上游悄悄少了收尾帧"（审查点名）。
		logStreamNoDone()
	}
	// 标记修复的尾部收尾（见 dsml.go）：
	//   - 流结束时仍未判定的尾部缓冲一律原文回吐（绝不吞字节：未闭合的块连起始标记一起
	//     交还）；
	//   - 本流还原出调用、但上游从未给出收尾帧时，补一帧 finish_reason: tool_calls，
	//     保证客户端不会把「工具调用回合」读成「模型只说了话」。
	if repairEnabled {
		if tail := repair.Flush(); tail != "" {
			tailObj := map[string]any{
				"object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": tail},
				}},
			}
			if werr := emitChunk(tailObj, ""); werr != nil {
				return werr
			}
		}
		if synthIdx > 0 {
			// 本流还原出了工具调用：还没写出的收尾一律按 tool_calls 处理，且恰好一个。
			// 上游把 finish_reason 重复贴在数据帧上时 remappedFinish 会把它挪进
			// pendingFinish、绕过 normalizeFrame 的 stop→tool_calls 改写，这里先提升；
			// pendingFinish 里没有的（上游压根不给收尾帧）则补一个，交给下面既有的
			// 「补末帧」块写出（它带的 log 正好说明这次是上游少给了收尾）。
			idxs := make([]int, 0, len(toolCallsByChoice))
			for cidx := range toolCallsByChoice {
				idxs = append(idxs, cidx)
			}
			sort.Ints(idxs)
			for _, cidx := range idxs {
				if finishEmitted[cidx] {
					continue // 该 choice 的收尾已写出，再补就是两个 finish_reason
				}
				if fr, ok := pendingFinish[cidx]; ok && fr != "stop" {
					continue // 已有语义更强的收尾（length / content_filter）→ 原样保留
				}
				pendingFinish[cidx] = "tool_calls"
			}
		}
	}
	// 若从来没出现过"收尾帧"（上游把 finish_reason 只贴在带内容的数据帧上），
	// 这里按规范补一个空 delta + finish_reason 的末帧，客户端才能正确判定结束。
	if len(pendingFinish) > 0 {
		idxs := make([]int, 0, len(pendingFinish))
		for i := range pendingFinish {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		chs := make([]any, 0, len(idxs))
		for _, i := range idxs {
			chs = append(chs, map[string]any{
				"index":         i,
				"delta":         map[string]any{},
				"finish_reason": pendingFinish[i],
			})
		}
		tail := map[string]any{
			"id":      firstID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   firstModel,
			"choices": chs,
		}
		if raw, err := json.Marshal(tail); err == nil {
			_ = writeRaw(string(raw))
		}
		log.Printf("WARN: [upstream] 上游未给规范的收尾帧（finish_reason 贴在数据帧上），已补 %d 个 choice 的末帧", len(idxs))
	}

	// 保证恰好写一个 [DONE]（上游漏发或异常中断时兜底补上，杜绝下游 Agent 挂死悬空）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	if streamErr != nil {
		return streamErr
	}
	return nil
}

// noDoneWarnEvery 静默截断（上游没发 [DONE] 就 EOF）日志节流：首次 + 每 N 次一条。
const noDoneWarnEvery = 100

var noDoneCount atomic.Int64

func logStreamNoDone() {
	n := noDoneCount.Add(1)
	if n == 1 || n%noDoneWarnEvery == 0 {
		log.Printf("WARN: [upstream] stream ended without [DONE]（静默截断，已兜底补 [DONE]；第 %d 次）", n)
	}
}

// StreamNoDoneCount 返回"上游没发 [DONE] 就 EOF"（静默截断）的累计次数。
// 供 /status 透出，运维无需翻日志即可发现"上游悄悄少了收尾帧"。
func StreamNoDoneCount() int64 { return noDoneCount.Load() }

// --- 标记修复的流式支撑（见 dsml.go 与 streamContext 的 repairFrame）----------

// maxToolCallIndex 返回上游本帧已用过的最大 tool_call index（没有则 -1）。
// 标记修复合成调用时从它之后接续编号，避免与真实调用撞 index —— 同 index 的两份调用
// 会被客户端的累加器揉成一个（见 markupToolCalls 注释）。
func maxToolCallIndex(obj map[string]any) int {
	max := -1
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			if v, ok := tc["index"].(float64); ok && int(v) > max {
				max = int(v)
			}
		}
	}
	return max
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则客户端
// 在读到调用之前就认为回合结束了。
//
// 取出后**不**立即补帧：上游的收尾帧随后就到，立刻补会产生两个 finish_reason。上游
// 若压根不给收尾帧，由 streamContext 流尾按需补一帧 tool_calls。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// frameFinishReason 返回已归一化帧上第一个非空 finish_reason（normalizeFrame 之后
// finish_reason 只可能是 string 或 nil）。
func frameFinishReason(frame map[string]any) string {
	chs, _ := frame["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			return fr
		}
	}
	return ""
}

// noteFinishedChoices 记录本帧哪些 choice 带出了收尾（供流尾判断还要不要兜底补帧）。
func noteFinishedChoices(frame map[string]any, emitted map[int]bool) {
	chs, _ := frame["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); !ok || fr == "" {
			continue
		}
		cidx := 0
		if v, ok := c["index"].(float64); ok {
			cidx = int(v)
		}
		emitted[cidx] = true
	}
}

// promoteToolFinish 把「该 choice 已有工具调用、finish_reason 仍为 stop」的收尾提升为
// tool_calls。normalizeFrame 已做过一次 stop→tool_calls，但 remappedFinish 会把上游
// 重复贴在数据帧上的 finish_reason 挪到收尾帧、绕过那一次归一化（见 remappedFinish），
// 这里在最终写出前再兜一次，保证「还原出工具调用的流」收尾恒为 tool_calls。
// 只改 stop：length / content_filter 这类语义更强的收尾原样保留。
func promoteToolFinish(frame map[string]any, toolCallsByChoice map[int]bool) {
	if len(toolCallsByChoice) == 0 {
		return
	}
	chs, _ := frame["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		cidx := 0
		if v, ok := c["index"].(float64); ok {
			cidx = int(v)
		}
		if !toolCallsByChoice[cidx] {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr == "stop" {
			c["finish_reason"] = "tool_calls"
		}
	}
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// cidx 是该调用所属的 choice.index（正文是从哪个 choice 里还原出来的就归给哪个）。
// id 不在这里写：交给 emitChunk 的 id 续传逻辑补齐（src 帧先写出，firstID 已就位）。
func markupCallFrame(src map[string]any, cidx, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": cidx,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}
