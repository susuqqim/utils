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
	// packagesTTL 积分构成页（CreditPackages）缓存时长。
	//
	// 这个接口是**按账号逐个打上游计费端点**的（5 个号并发 3 → 实测稳定 ~1.0s），
	// 而面板上它只是一个"看看积分包何时到期"的只读展示页。此前每切一次页就打 5 次
	// 上游、等 1 秒——既慢又白烧请求（与刚修的模型目录缓存是同一类问题，当时漏了它）。
	// 积分包到期以天计，5 分钟足够新。
	packagesTTL = 5 * time.Minute

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

// packagesCacheEntry 积分构成页的缓存条目。
type packagesCacheEntry struct {
	payload map[string]any
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

// cachedPackages 读积分构成缓存。
func (p *Panel) cachedPackages(now time.Time) (map[string]any, bool) {
	p.packagesMu.Lock()
	defer p.packagesMu.Unlock()
	if p.packagesCache == nil || now.Sub(p.packagesAt) >= packagesTTL {
		return nil, false
	}
	return p.packagesCache, true
}

// storePackages 写积分构成缓存。
func (p *Panel) storePackages(payload map[string]any, now time.Time) {
	p.packagesMu.Lock()
	defer p.packagesMu.Unlock()
	p.packagesCache = payload
	p.packagesAt = now
}

// staleModels 取**过期但仍在**的模型目录（不看 TTL）。
//
// 【2026-10-21 生产实测事故】只有 TTL 判定时，缓存一过期就必须同步打上游；
// 上游 TLS 握手一旦抖动（实测 `net/http: TLS handshake timeout`），
// 面板「模型与档位」页会**卡死 30 秒以上**（客户端直接超时），而其它接口都在毫秒级。
// 只读展示页不该被上游抖动拖死 ⇒ 过期后先回旧数据，后台异步刷新。
func (p *Panel) staleModels(key string) (modelsCacheEntry, bool) {
	p.modelsMu.Lock()
	defer p.modelsMu.Unlock()
	e, ok := p.modelsCache[key]
	if !ok {
		return modelsCacheEntry{}, false
	}
	return e, true
}

// stalePackages 取过期的积分构成数据（语义同 staleModels）。
func (p *Panel) stalePackages() (map[string]any, bool) {
	p.packagesMu.Lock()
	defer p.packagesMu.Unlock()
	if p.packagesCache == nil {
		return nil, false
	}
	return p.packagesCache, true
}

// beginRefresh 尝试取得某个 key 的后台刷新权（同一时刻只允许一个在跑）。
// 返回 false 表示已有刷新在途，调用方直接复用旧数据即可，不必再排一个 goroutine。
func (p *Panel) beginRefresh(key string) bool {
	p.refreshingMu.Lock()
	defer p.refreshingMu.Unlock()
	if p.refreshing == nil {
		p.refreshing = map[string]bool{}
	}
	if p.refreshing[key] {
		return false
	}
	p.refreshing[key] = true
	return true
}

// endRefresh 释放刷新权。
func (p *Panel) endRefresh(key string) {
	p.refreshingMu.Lock()
	defer p.refreshingMu.Unlock()
	delete(p.refreshing, key)
}

// wantRefresh 报告请求是否要求穿透缓存（?refresh=1）。
func wantRefresh(r *http.Request) bool {
	v := r.URL.Query().Get("refresh")
	return v == "1" || v == "true"
}
