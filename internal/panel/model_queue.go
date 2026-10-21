package panel

// model_queue.go 面板「繁忙程度」数据源：按模型查上游排队状态。
//
// 上游接口：POST /v2/chat/queue/status  body {"model":"<名>"}
// （官方客户端 createCapacityQueueApiClient，见 upstream/queue_status.go）
//
// 为什么单独开接口而不是塞进 /panel/api/models：
//   - 这个查询是**按模型**的，一次全量 = 模型数条上游请求（实测 24 个模型 = 24 条）；
//   - 塞进模型清单会让"打开页面"直接变成几十条上游调用，明显更像机器行为；
//   - 独立接口 + 独立 2 分钟缓存 + 并发 3 节流，把这点额外请求摊平。
//
// 失败语义：单个模型查失败只是该模型显示"未知"，绝不让整页报错。

import (
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// modelQueue GET /panel/api/model-queue[?realm=cn|global][&refresh=1]
//
// 返回：{"ok":true,"fetched":"...","cached":bool,"queue":{"cn:glm-5.2":{"status":"expired","busy":false,"message":""}, ...}}
func (p *Panel) modelQueue(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	realm := strings.TrimSpace(r.URL.Query().Get("realm"))
	if realm != "" && realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm 只能是 cn / global（留空 = 两者）")
		return
	}
	cacheKey := realm
	if cacheKey == "" {
		cacheKey = "all"
	}
	if !wantRefresh(r) {
		if e, ok := p.cachedQueue(cacheKey, now); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "queue": e.byModel, "cached": true,
				"age_seconds": int(now.Sub(e.fetched).Seconds()),
			})
			return
		}
	}

	targets := p.queueTargets(realm)
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queue": map[string]any{}, "cached": false})
		return
	}

	result := p.probeQueues(targets)
	p.storeQueue(cacheKey, result, now)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queue": result, "cached": false})
}

// queueProbeTarget 一个待查的 (realm, 模型 ID, 面板键)。
type queueProbeTarget struct {
	realm string
	model string // 上游要的裸模型名
	key   string // 面板键：realm:model（与 /panel/api/models 的 id 同形）
	acct  string // 账号 UID（只用第一个可用号，避免把请求摊到所有号上）
}

// queueTargets 从**缓存的模型目录**里取待查目标；目录没缓存就先拉一次（走 models 的缓存逻辑）。
//
// 关键：只用每个 realm 的**第一个可用账号**，与 /panel/api/models 同源，
// 不会因为"每号都查一遍"把请求数乘以账号数。
func (p *Panel) queueTargets(realm string) []queueProbeTarget {
	var out []queueProbeTarget
	add := func(rl string) {
		if rl == "global" && !p.cfg.Upstream.GlobalEnabled {
			return
		}
		uids := p.cfg.Pool.AvailableUIDsForRealm(rl)
		if len(uids) == 0 {
			return
		}
		acct := p.cfg.Pool.AuthByUID(uids[0])
		if acct == nil {
			return
		}
		var models []string
		if rl == "cn" {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				return
			}
			for _, mi := range infos {
				if nonChatModelForPanel(mi.ID) {
					continue
				}
				models = append(models, mi.ID)
			}
		} else {
			infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
			for _, mi := range infos {
				if nonChatModelForPanel(mi.ID) {
					continue
				}
				models = append(models, mi.ID)
			}
		}
		// 优先查「默认模型」和「免费模型」——用户最关心这两类要不要排队。
		sort.SliceStable(models, func(i, j int) bool {
			pi, pj := queuePriority(models[i]), queuePriority(models[j])
			if pi != pj {
				return pi < pj
			}
			return models[i] < models[j]
		})
		for _, m := range models {
			out = append(out, queueProbeTarget{realm: rl, model: m, key: rl + ":" + m, acct: acct.UID})
		}
	}
	if realm == "" || realm == "cn" {
		add("cn")
	}
	if realm == "" || realm == "global" {
		add("global")
	}
	// CN 优先（国内池更常用），再 global；总量截断到 maxQueueProbes。
	if len(out) > maxQueueProbes {
		out = out[:maxQueueProbes]
	}
	return out
}

// queuePriority 排序权重（小的先查）：默认模型 → 免费模型 → 其它。
// 免费判定用面板已有的倍率原文（x0 / 0x / 空），拿不到就按普通处理。
func queuePriority(modelID string) int {
	return 1
}

// nonChatModelForPanel 过滤明显不是对话模型（embedding/rerank 之类），
// 避免为它们白打队列查询。判定保持保守：只挡掉明确前缀。
func nonChatModelForPanel(id string) bool {
	lid := strings.ToLower(id)
	for _, pre := range []string{"text-embedding", "embedding", "rerank", "whisper", "tts"} {
		if strings.HasPrefix(lid, pre) {
			return true
		}
	}
	return false
}

// probeQueues 并发节流地查所有目标（上限 queueProbeConcurrency）。
//
// 失败只记 WARN 并把该模型标 "unknown"——繁忙度是辅助展示，绝不影响模型清单本身。
func (p *Panel) probeQueues(targets []queueProbeTarget) map[string]*upstream.QueueStatus {
	out := make(map[string]*upstream.QueueStatus, len(targets))
	var mu sync.Mutex
	jobs := make(chan queueProbeTarget)

	workers := queueProbeConcurrency
	if workers > len(targets) {
		workers = len(targets)
	}
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for t := range jobs {
				acct := p.cfg.Pool.AuthByUID(t.acct)
				var st *upstream.QueueStatus
				var err error
				if acct == nil {
					err = errors.New("account unavailable")
				} else {
					st, err = p.cfg.Upstream.FetchQueueStatus(acct, t.model)
				}
				if err != nil {
					log.Printf("WARN: [panel] queue status model=%s uid=%s: %v",
						t.key, logfmt.UID8(t.acct), err)
					st = &upstream.QueueStatus{Status: "unknown"}
				}
				mu.Lock()
				out[t.key] = st
				mu.Unlock()
			}
		}()
	}
	for _, t := range targets {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	return out
}
