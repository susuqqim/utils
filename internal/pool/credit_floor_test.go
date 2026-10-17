package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 积分保底（上游 d19add4）：余额低于保底的账号不接**实测收费**模型；
// 免费模型、无观测模型、保底关闭时一律放行；兜底与分块两条旁路也必须受约束。
func creditPool(t *testing.T, credits map[string]int64) *Pool {
	t.Helper()
	p := New(filepath.Join(t.TempDir(), "state.json"))
	for uid, c := range credits {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
		p.SetCredits(uid, c)
	}
	return p
}

func TestCreditFloorBlocksObservedPaidModel(t *testing.T) {
	p := creditPool(t, map[string]int64{"poor": 100})
	p.SetCreditFloor(150)
	p.NoteModelCost("poor", "glm-5.2", 5.0, 1000) // 实测收费
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a != nil {
		t.Fatalf("触底号不该接实测收费模型，实得 %s", a.UID)
	}
	// 恰好等于保底 → 放行（判据是 credits < floor）
	p.SetCredits("poor", 150)
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a == nil {
		t.Error("credits == floor 应放行")
	}
}

func TestCreditFloorAllowsFreeAndUnobserved(t *testing.T) {
	p := creditPool(t, map[string]int64{"poor": 100})
	p.SetCreditFloor(150)
	// 实测免费（CostPer1k == 0）→ 放行
	p.NoteModelCost("poor", "hy3", 0, 1000)
	if a := p.PickExcludingForModel(nil, "hy3"); a == nil {
		t.Error("实测免费模型不该被保底拦（否则等于变相禁用触底号）")
	}
	// 无观测 → 放行（tier1 语义：不惩罚未知）
	if a := p.PickExcludingForModel(nil, "unknown-model"); a == nil {
		t.Error("无观测模型不该被保底拦")
	}
}

func TestCreditFloorZeroDisables(t *testing.T) {
	p := creditPool(t, map[string]int64{"poor": 1})
	p.NoteModelCost("poor", "glm-5.2", 9.9, 1000)
	// 默认 0 = 关闭（零回归）
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a == nil {
		t.Error("保底未开启时不该改变选号")
	}
	// 负值不修改（沿用 SetMaxInFlight 语义）
	p.SetCreditFloor(-5)
	if p.CreditFloor() != 0 {
		t.Errorf("负值应不修改，实得 %d", p.CreditFloor())
	}
}

// 全冷却兜底路径（上游 b98fb4f）：保底挡掉健康候选后，不能让触底号被"捞回来"。
func TestCreditFloorFallbackRespected(t *testing.T) {
	p := creditPool(t, map[string]int64{"poor": 100})
	p.SetCreditFloor(150)
	p.NoteModelCost("poor", "glm-5.2", 5.0, 1000)
	p.Cooldown("poor", CoolSoft, time.Minute, "soft rate") // 逼进兜底路径
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a != nil {
		t.Fatalf("兜底路径也不该捞回触底号，实得 %s", a.UID)
	}
	// 免费模型仍应被兜底放行（保底只管收费模型）
	p.NoteModelCost("poor", "hy3", 0, 1000)
	if a := p.PickExcludingForModel(nil, "hy3"); a == nil {
		t.Error("兜底路径对免费模型应照常放行")
	}
}

// 本地专有旁路（上游没有）：Phase 3 分块路径**直取** activeChunks 里的账号，
// 绕过候选过滤；分块内每笔实扣余额，账号会中途跌破保底。
// 回归护栏：分块账号触底后，同模型的下一次选号必须改选其他账号（而不是继续烧它）。
func TestCreditFloorChunkPathExcluded(t *testing.T) {
	p := creditPool(t, map[string]int64{"rich": 500, "poor": 400})
	pol := p.SchedulePolicy()
	pol.ChunkQuotaCredits = 25 // 开启分块调度（Phase 3 主刷）
	p.SetSchedulePolicy(pol)
	p.SetCreditFloor(150)
	p.NoteModelCost("rich", "glm-5.2", 9.9, 1000)
	p.NoteModelCost("poor", "glm-5.2", 9.9, 1000)

	first := p.PickExcludingForModel(nil, "glm-5.2")
	if first == nil {
		t.Fatal("首次应能选出账号")
	}
	chunkUID := first.UID
	// 把分块账号打到保底以下（模拟分块内实扣）
	p.SetCredits(chunkUID, 100)

	second := p.PickExcludingForModel(nil, "glm-5.2")
	if second == nil {
		t.Fatal("另一个账号仍健康，应能选出")
	}
	if second.UID == chunkUID {
		t.Errorf("分块账号(%s)已触底，不该继续被分块路径直取", chunkUID)
	}
}

// 目录倍率兜底（上游 39af6b7）：本地尚无实测成本时用目录价判断是否收费，
// 堵住"全池无观测 → 全放行被高价新模型一笔打穿"。未知一律放行（不惩罚）。
func TestCreditFloorCatalogRateFallback(t *testing.T) {
	rates := map[string]map[string]string{
		"": {"glm-5.2": "1.62", "hy3": "0.00", "alias-unknown": ""},
	}
	p := creditPool(t, map[string]int64{"poor": 100})
	p.SetCreditFloor(150)
	p.SetModelRateOf(func(realm, model string) string { return rates[realm][model] })

	// 无实测 + 目录判收费 → 拦（必须单账号池，避免权重/随机源造成假绿）
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a != nil {
		t.Errorf("无实测但目录判收费时应拦触底号，实得 %s", a.UID)
	}
	// 目录判免费 → 放行
	if a := p.PickExcludingForModel(nil, "hy3"); a == nil {
		t.Error("目录判免费应放行")
	}
	// 目录未覆盖 → 放行（未知不惩罚）
	if a := p.PickExcludingForModel(nil, "never-heard-of"); a == nil {
		t.Error("目录未覆盖应放行")
	}
	// 实测优先于目录：实测免费 + 目录收费 → 放行
	p.NoteModelCost("poor", "glm-5.2", 0, 1000)
	if a := p.PickExcludingForModel(nil, "glm-5.2"); a == nil {
		t.Error("实测免费应优先于目录收费")
	}
	// 未注入回调（旧行为）→ 放行
	p2 := creditPool(t, map[string]int64{"poor": 100})
	p2.SetCreditFloor(150)
	if a := p2.PickExcludingForModel(nil, "glm-5.2"); a == nil {
		t.Error("未注入目录回调时应退化为只认实测（放行）")
	}
}
