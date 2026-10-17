// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/cstime"
	"workbuddy2api/internal/gosafe"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// flushFailLogEvery 连续落盘失败每 N 次打一条提醒（flush 间隔 30s，首败已详报，
// 持续不可写时不必每 30s 刷屏）。
const flushFailLogEvery = 20

// cstZone 固定 +08:00 时区：与池侧日切（硬冷却/跨日重置/签到窗口）**同口径**。
// 用固定偏移而非 time.LoadLocation("Asia/Shanghai")：容器常缺 tzdata，加载失败会 panic/降级。
//
// 为什么不用宿主机本地时区（2026-10-03 修）：产品语义里的"自然日"是中国区自然日
// （跨日重置、签到、积分日切全部按 CST），而 Docker 默认 UTC → 原实现会把桶键落在 UTC 日界，
// 与中国区日界差 8 小时（同一个"今天"在面板上被劈成两天）。本机 Windows 时区正是 CST，
// 故该修复对当前部署零行为变化，只在 UTC/其它时区宿主上纠正口径。
// cstZone 见 internal/cstime：全仓唯一口径（桶键生成与解析必须同一口径）。
var cstZone = cstime.Zone

// hourLayout / dayLayout 分片键的时间格式（**CST 口径**，见 cstZone）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope  string  `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm  string  `json:"r"`
	UID    string  `json:"u"`
	Model  string  `json:"m"`
	Req    int64   `json:"q"`            // 请求数（含失败）
	Err    int64   `json:"e"`            // 失败数
	PT     int64   `json:"p"`            // prompt tokens
	CT     int64   `json:"c"`            // completion tokens
	TT     int64   `json:"t"`            // total tokens（上游给什么用什么的合计）
	CC     int64   `json:"cc,omitempty"` // 缓存命中 prompt tokens（prompt_tokens_details.cached_tokens）
	Credit float64 `json:"cr,omitempty"` // 积分消耗累计
	LatMs  int64   `json:"l"`            // 延迟累计（ms）
	LatN   int64   `json:"ln"`           // 延迟样本数
	TPS    float64 `json:"v"`            // 吐字速率累计
	TPSN   int64   `json:"vn"`           // 速率样本数
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	// flushFails 连续落盘失败计数（仅持 mu 时读写）：用于节流日志与"恢复"提示。
	flushFails int
	started    time.Time

	// flushMu 串行化并发 flush：30s 定时器与面板「刷新」（Save）可能同时进来，
	// 两者共用 <path>.tmp —— 交错会让 rename 搬走半截文件，随后 load() 解析失败
	// 即"从零开始"，把用量历史覆盖掉（审查指出，文件系统层面 -race 覆盖不到）。
	flushMu sync.Mutex

	// startOnce 防止 Start() 被调用两次：重复执行会二次 close(r.done) → panic → 进程退出
	// （当前唯一调用点只调一次，但热重载/测试复用很容易踩到）。
	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	r.startOnce.Do(func() {
		r.startLoop()
	})
}

// startLoop 启动落盘循环（只应由 Start 的 once 调用一次）。
func (r *Recorder) startLoop() {
	go gosafe.Do("usage flusher", func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				// 迭代级兜底：顶层 recover 会让落盘循环结束 → 用量历史永久停止落盘。
				gosafe.Loop("usage flusher", func() {
					r.mu.Lock()
					n := len(r.buckets)
					r.mu.Unlock()
					if n > maxBuckets {
						r.Rollup(time.Now())
					}
					r.flush(false)
				})
			}
		}
	})
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	CachedTokens     int64
	HasCached        bool
	// Canceled 客户端主动取消（499）：计入请求数但不计入失败（账号无过错）。
	Canceled        bool
	Credit          float64
	HasCredit       bool
	LatencyMs       int64
	HasLatency      bool
	TokensPerSecond float64
	HasTPS          bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.In(cstZone).Format(hourLayout)
	// 键生成收敛到 group.go（S4 方案 A）：全仓单一实现，group_test.go 的两条陷阱用例
	// （双域同名模型不得合并、CST 自然日边界）即成为全仓守卫。
	key := BucketKey(scope, realm, uid, model)

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model}
		r.buckets[key] = b
	}
	b.Req++
	if !ok && !d.Canceled {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasCached && d.CachedTokens > 0 {
		b.CC += d.CachedTokens
	}
	if d.HasCredit && d.Credit > 0 {
		b.Credit += d.Credit
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), cstZone) // 与写入口径一致（此前用宿主本地时区）
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.In(cstZone).Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			// CC（缓存命中）必须与其它字段同口径累加：漏掉它会让同一天里"第二个及以后
			// 被折叠的小时桶"的命中量凭空消失，面板命中率随数据老化而系统性偏低。
			dst.CC += src.CC
			dst.Credit += src.Credit
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		r.failFlush("序列化失败", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		r.failFlush("建目录失败", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		r.failFlush("写临时文件失败", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		// 回退直接覆盖写（单文件挂载、跨设备或 Windows 句柄占用）
		if werr := os.WriteFile(r.path, raw, 0o600); werr != nil {
			r.failFlush("原子替换及直接覆盖写均失败", fmt.Errorf("rename=%v write=%v", err, werr))
			return
		}
		_ = os.Remove(tmp)
	}
	r.okFlush()
}

// failFlush 落盘失败统一出口：**回置 dirty**（下一轮 flush 重试，而不是静默丢掉这一轮的
// 全部用量增量）+ 节流日志（首次详报，其后每 flushFailLogEvery 次一条，避免磁盘持续
// 不可写时刷屏）。
func (r *Recorder) failFlush(stage string, err error) {
	r.mu.Lock()
	r.dirty = true
	r.flushFails++
	n := r.flushFails
	r.mu.Unlock()
	if n == 1 || n%flushFailLogEvery == 0 {
		log.Printf("[usage] %s: %v（第 %d 次连续失败，已回置待下轮重试）", stage, err, n)
	}
}

// okFlush 落盘成功后清零失败计数；从连续失败中恢复时打一条恢复日志。
func (r *Recorder) okFlush() {
	r.mu.Lock()
	n := r.flushFails
	r.flushFails = 0
	r.mu.Unlock()
	if n > 0 {
		log.Printf("[usage] 用量落盘恢复（此前连续失败 %d 次）", n)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	Credits       float64 `json:"credits"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.CachedTokens += b.CC
	g.Credits += b.Credit
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key              string  `json:"key"`
	Realm            string  `json:"realm,omitempty"`
	Extra            string  `json:"extra,omitempty"` // 账号行放昵称
	DailyCreditsUsed float64 `json:"daily_credits_used,omitempty"`
	TotalCreditsUsed float64 `json:"total_credits_used,omitempty"`
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals                Agg        `json:"totals"`
	ByRealm               []KeyedAgg `json:"by_realm"`
	ByAccount             []KeyedAgg `json:"by_account"`
	ByModel               []KeyedAgg `json:"by_model"`
	Series                []Point    `json:"series"`
	Buckets               int        `json:"buckets"`
	FileBytes             int64      `json:"file_bytes"`
	Since                 string     `json:"since,omitempty"`
	Generated             string     `json:"generated"`
	TotalDailyCreditsUsed float64    `json:"total_daily_credits_used,omitempty"`
	TotalCreditsUsed      float64    `json:"total_credits_used,omitempty"`
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	windowed := hours > 0
	if windowed && hours > 24*60 {
		hours = 24 * 60
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}

	var hourFrom time.Time
	if windowed {
		nowHour := time.Now().Truncate(time.Hour)
		hourFrom = nowHour.Add(-time.Duration(hours-1) * time.Hour)
	}

	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if b.Scope < since || since == "" {
			since = b.Scope
		}
		if windowed {
			var ts time.Time
			var err error
			if strings.HasPrefix(b.Scope, "h:") {
				ts, err = time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), cstZone)
			} else {
				ts, err = time.ParseInLocation(dayLayout, strings.TrimPrefix(b.Scope, "d:"), cstZone)
			}
			// 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
			if err != nil || ts.Before(hourFrom) {
				continue
			}
		}
		matched++
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		if strings.HasPrefix(b.Scope, "h:") {
			scope := strings.TrimPrefix(b.Scope, "h:")
			if hourSeries[scope] == nil {
				hourSeries[scope] = &aggAcc{}
			}
			hourSeries[scope].add(b)
		} else {
			scope := strings.TrimPrefix(b.Scope, "d:")
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:   keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		Buckets:   matched,
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-09-16T13" → "2026-09-16T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	return snap
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
