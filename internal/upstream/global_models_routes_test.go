package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// 桌面端主路必须用项目**正典** UA builder（审查 A/C 的 P0/P1-1）：
// 平台段是第二段（`WorkBuddy/<cv> <platform>/<cv> CLI/<cli>`），global 用 `WorkBuddy AI`。
// 早先自造的 strings.Replace(...,1) 会把品牌段塞进第一段（`WorkBuddy AI/5.5.6 WorkBuddy/…`），
// 与 chat 路径 UA 不同源 —— 这条断言正是"改错就红"。
func TestDesktopUAForGlobalMatchesCanonical(t *testing.T) {
	auth.SetGlobalEnabled(true)
	// 与 headers_global_test.go 的 globalUAString 同形（版本段读 clientVersion()）
	got := (&Client{}).defaultWorkBuddyUAFor(&auth.Auth{UID: "g", Domain: "www.workbuddy.ai"})
	if got != globalUAString {
		t.Errorf("global 桌面 UA = %q，期望正典形态 %q（平台段必须在第二段）", got, globalUAString)
	}
	// 反向：不得是"品牌段在第一段"的倒置形态
	if got == "WorkBuddy AI/"+defaultClientVersion+" WorkBuddy/"+defaultClientVersion+" CLI/"+cliVersionOrDefaultForTest() {
		t.Error("UA 出现品牌段倒置（第一段带 AI）")
	}
}

func cliVersionOrDefaultForTest() string { return "2.137.1" }

// 三路合并（上游 7b24e22）：首路成功者字段权威，后续路补缺失 id **并回填能力字段**。
func TestMergeV3RoutesPriorityAndBackfill(t *testing.T) {
	mk := func(id string, ctx int64, efforts []string) ModelInfo {
		return ModelInfo{ID: id, ContextWindow: ctx, Efforts: efforts}
	}
	desktop := probeResult{
		names: []string{"shared", "desktop-only"},
		infos: []ModelInfo{mk("shared", 100000, nil), mk("desktop-only", 200000, nil)},
	}
	ide := probeResult{
		names: []string{"shared", "ide-only"},
		infos: []ModelInfo{mk("shared", 128000, []string{"low", "high"}), mk("ide-only", 64000, nil)},
	}
	got := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{{"desktop-UA", desktop}, {"IDE-UA", ide}})
	if got.err != nil {
		t.Fatalf("不该报错: %v", got.err)
	}
	if len(got.infos) != 3 {
		t.Fatalf("模型数=%d want 3（并集）", len(got.infos))
	}
	byID := map[string]ModelInfo{}
	for _, mi := range got.infos {
		byID[mi.ID] = mi
	}
	if byID["shared"].ContextWindow != 100000 {
		t.Errorf("同 id 应以主路字段为准：ctx=%d want 100000", byID["shared"].ContextWindow)
	}
	if len(byID["shared"].Efforts) == 0 {
		t.Error("主路缺 supportedEfforts 时必须由后续路回填（否则思考档位降级桶静默缩水）")
	}
	if _, ok := byID["ide-only"]; !ok {
		t.Error("后续路独有 id 必须补进并集")
	}
}

// 部分失败：warn 后继续用存活路；全失败才带 err（负缓存语义不变）。
func TestMergeV3RoutesPartialAndTotalFailure(t *testing.T) {
	ok := probeResult{names: []string{"m1"}, infos: []ModelInfo{{ID: "m1"}}}
	bad := probeResult{err: errString("boom")}
	partial := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{{"desktop-UA", bad}, {"IDE-UA", ok}})
	if partial.err != nil || len(partial.names) != 1 {
		t.Errorf("部分失败应降级为存活路：err=%v names=%v", partial.err, partial.names)
	}
	all := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{{"desktop-UA", bad}, {"IDE-UA", bad}})
	if all.err == nil {
		t.Error("全失败必须带 err（否则会把空目录当成功缓存 1h）")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// 生成类标签必须被过滤（含大小写/空白差异 —— 审查 A/C 的 P2-6）。
func TestNonChatModelGenerationTags(t *testing.T) {
	for _, tag := range []string{"text-to-image", "image-to-image", "text-to-video", "image-to-video", " Text-To-Video "} {
		if !nonChatModel("some-normal-id", 4096, []string{tag}) {
			t.Errorf("tag %q 应判为非对话模型", tag)
		}
	}
	// 正常对话模型不该被误伤
	if nonChatModel("glm-4.6v", 4096, []string{"vision", "chat"}) {
		t.Error("vision/chat 标签的正常模型不该被过滤")
	}
}

// partial 标记：有路失败必须置位（上游侧据此与上一份全路成功目录取并集，
// 防止"一次瞬时 5xx → 缩水目录被当成功缓存 1h"；审查 C 的 P1-2）。
func TestMergeV3RoutesPartialFlag(t *testing.T) {
	ok := probeResult{names: []string{"m1"}, infos: []ModelInfo{{ID: "m1"}}}
	bad := probeResult{err: errString("boom")}
	full := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{{"a", ok}, {"b", ok}})
	if full.partial {
		t.Error("全路成功不该置 partial（否则基线 goodInfos 永不刷新）")
	}
	part := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{{"a", ok}, {"b", bad}})
	if !part.partial {
		t.Error("有路失败必须置 partial")
	}
}
