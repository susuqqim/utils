package panel

import (
	"net/http"
	"time"

	"workbuddy2api/internal/upstream"
)

// model_cache.go 面板侧的两级缓存：模型目录 + 排队（繁忙度）。
//
// 背景（2026-10-21）：`/panel/api/models` 每次调用都会向上游打 2~3 次 /v3/config
// （CN 走 IDE+CLI 双 UA 并集，再加 global 独立目录），面板每切一次页就烧一轮；
// 而「繁忙度」是**按模型**查询的，全量一刷就是模型数条请求。
// 两者都必须缓存，否则页面越刷越像机器行为。

const (
	// catalogTTL 模型目录缓存时长。目录变化很慢（上游改动以天计），
	// 10 分钟足够新；用户点「重新获取」可强制穿透。
	catalogTTL = 10 * time.Minute
	// queueTTL 排队状态缓存时长。状态变化快但页面展示不需要秒级精度，
	// 2 分钟能挡住连续切页/多标签页的重复查询。
	queueTTL = 2 * time.Minute
	// queueProbeConcurrency 查排队时的并发上限：串行太慢，全并发又太像扫描，取 4。
	queueProbeConcurrency = 4
	// maxQueueProbes 单次「查繁忙」最多查多少个模型。
	//
	// 实测目录 69 个模型（cn 35 + global 34），全查就是 69 条上游请求——
	// 集中在一分钟内打完，与其说是查询不如说是扫描。限 24 个，
	// 并按「CN 优先、默认模型优先」排序取前 24，覆盖用户最关心的那批。
	maxQueueProbes = 24
)

// modelsCacheEntry 一个 realm 的模型目录缓存条目。
type modelsCacheEntry struct {
	entries []map[string]any
	errs    []string
	fetched time.Time
}

// queueCacheEntry 一个 realm 的排队状态缓存条目（模型名 → 状态）。
type queueCacheEntry struct {
	byModel map[string]*upstream.QueueStatus
	fetched time.Time
}

// cachedModels 读缓存；fresh=true 表示命中且未过期。
func (p *Panel) cachedModels(realm string, now time.Time) (modelsCacheEntry, bool) {
	p.modelsMu.Lock()
	defer p.modelsMu.Unlock()
	e, ok := p.modelsCache[realm]
	if !ok || now.Sub(e.fetched) >= catalogTTL {
		return modelsCacheEntry{}, false
	}
	return e, true
}

// storeModels 写缓存（entries 为 nil 时不覆盖，避免把一次失败的结果固化 10 分钟）。
func (p *Panel) storeModels(realm string, entries []map[string]any, errs []string, now time.Time) {
	if entries == nil {
		return
	}
	p.modelsMu.Lock()
	defer p.modelsMu.Unlock()
	if p.modelsCache == nil {
		p.modelsCache = make(map[string]modelsCacheEntry)
	}
	p.modelsCache[realm] = modelsCacheEntry{entries: entries, errs: errs, fetched: now}
}

// cachedQueue 读排队缓存。
func (p *Panel) cachedQueue(realm string, now time.Time) (queueCacheEntry, bool) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	e, ok := p.queueCache[realm]
	if !ok || now.Sub(e.fetched) >= queueTTL {
		return queueCacheEntry{}, false
	}
	return e, true
}

// storeQueue 写排队缓存。
func (p *Panel) storeQueue(realm string, byModel map[string]*upstream.QueueStatus, now time.Time) {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	if p.queueCache == nil {
		p.queueCache = make(map[string]queueCacheEntry)
	}
	p.queueCache[realm] = queueCacheEntry{byModel: byModel, fetched: now}
}

// wantRefresh 报告请求是否要求穿透缓存（?refresh=1）。
func wantRefresh(r *http.Request) bool {
	v := r.URL.Query().Get("refresh")
	return v == "1" || v == "true"
}
