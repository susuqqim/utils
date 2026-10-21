// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/cstime"
	"workbuddy2api/internal/gosafe"
	"workbuddy2api/internal/reqlog"

	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	// APIKeyPrevious 过渡密钥（轮换期）：非空时也被接受，见 httpauth.VerifyBearerTransition。
	APIKeyPrevious string
	RedisMode      string // "upstash" / "noop"，仅观测透出
	Version        string // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// Restart 触发网关服务重启；nil 时返回 501。
	// TryCheckinLock/UnlockCheckin 与排程 CheckinAll 共用一把"签到中"锁（可空 = 不加锁）。
	// 单号签到会改当天签到状态并打上游，必须与定时全量签到互斥（第十轮审计 F5）。
	TryCheckinLock func() bool
	UnlockCheckin  func()

	Restart func() error

	// Shutdown 触发网关服务关闭；nil 时返回 501。
	Shutdown func() error
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	// S3 请求指标（只读视图数据源）：由 main 在启用时注入；零值 = 未启用。
	// 只持引用，不复制数据；读取走快照/归档读接口，天然并发安全。
	reqLogRing *reqlog.Ring
	reqLogArch *reqlog.Archive
	cfg        Config
	mux        *http.ServeMux
	started    time.Time
	logs       *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// modelsMu/modelsCache 模型目录缓存（2026-10-21）：
	// /panel/api/models 此前**每次**都向上游发 2~3 次 /v3/config（CN 双 UA + global），
	// 面板每切一次页就烧一轮上游请求，既慢又平添风控暴露。
	// 现在按 realm 缓存 catalogTTL，`?refresh=1` 强制穿透。
	modelsMu    sync.Mutex
	modelsCache map[string]modelsCacheEntry

	// refreshingMu/refreshing 后台刷新权（按 key），防同一份数据被并发重复刷新。
	refreshingMu sync.Mutex
	refreshing   map[string]bool

	// packagesMu/packagesCache 积分构成页缓存：该接口按账号逐个打上游计费端点
	//（5 个号 ≈ 1.0s），而页面只是只读展示；5 分钟内复用，避免每切页就烧一轮上游。
	packagesMu    sync.Mutex
	packagesCache map[string]any
	packagesAt    time.Time

	// queueMu/queueCache 排队（繁忙度）缓存：/v2/chat/queue/status 是**按模型**的，
	// 一次全量查询 = 模型数条上游请求，必须长缓存 + 并发节流，否则刷新一次就打出几十条。
	queueMu    sync.Mutex
	queueCache map[string]queueCacheEntry

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState
}

// SetReqLog 注入请求指标数据源（S3；两者皆可为 nil = 面板显示"未启用"）。
func (p *Panel) SetReqLog(ring *reqlog.Ring, arch *reqlog.Archive) {
	p.reqLogRing, p.reqLogArch = ring, arch
}

// reqLog 面板「请求指标」只读接口：默认返回内存环最近 limit 条（升序）；
// since 为 RFC3339 时改读**归档**（按事件时间排序，上游 7d462bb 语义）。
func (p *Panel) reqLog(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	source := "ring"
	var entries []reqlog.Entry
	sinceErr := ""
	if sv := r.URL.Query().Get("since"); sv != "" {
		// 边界（补审发现）：since 非法 / 未启用归档时**不能静默装作读了归档** ——
		// 调用方以为拿到的是历史数据、实际拿到的是内存环，会得出错误结论。
		ts, err := time.Parse(time.RFC3339, sv)
		switch {
		case err != nil:
			sinceErr = "since 解析失败（需 RFC3339，如 2026-10-09T00:00:00Z）；已退回内存环"
		case p.reqLogArch == nil:
			sinceErr = "未启用磁盘归档（reqlog.dir 为空）；已退回内存环"
		default:
			source = "archive"
			entries, _ = p.reqLogArch.Read(ts, limit)
		}
	}
	if source == "ring" {
		if p.reqLogRing == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "source": "ring", "rows": []reqlog.Entry{}, "count": 0})
			return
		}
		all := p.reqLogRing.Snapshot()
		if len(all) > limit {
			all = all[len(all)-limit:]
		}
		entries = all
	}
	files, bytes := 0, int64(0)
	if p.reqLogArch != nil {
		files, bytes = p.reqLogArch.Stats()
	}
	resp := map[string]any{
		"enabled": p.reqLogRing != nil || p.reqLogArch != nil,
		"source":  source, "rows": entries, "count": len(entries),
		"limit":         limit, // 回显**生效**值：非法/超限时是退回值，调用方不必猜
		"archive_files": files, "archive_bytes": bytes,
	}
	if sinceErr != "" {
		resp["since_error"] = sinceErr
	}
	writeJSON(w, http.StatusOK, resp)
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	// ⚠️ 新增/改动路由时必做两件事（第九轮审计 F1）：
	//  1. 若该路由**会改状态**（写盘/入池/触发上游写），且方法是 GET，必须同步进
	//     `server.isPanelStateChanging`（keyless 模式下"改状态请求需自定义头"的白名单）——
	//     `login/poll` 就是先例：它是 GET 却会写 auths + 入池 + 触发签到/试用，靠 `<img>` 即可驱动。
	//  2. 同步更新 `TestPanelRouteInventory`（路由清单守卫）：清单变化即测试变红，强制人工复核。
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	// 排队/繁忙度（按模型查上游 /v2/chat/queue/status，独立 2 分钟缓存 + 并发 3）
	p.mux.HandleFunc("GET /panel/api/model-queue", p.withAuth(p.modelQueue))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	// 暂停选号（上游 fd835df）：与"禁用"正交 —— 退出选号但保号任务照常，可随时恢复。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pin", p.withAuth(p.accountPin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/unpin", p.withAuth(p.accountUnpin))
	p.mux.HandleFunc("POST /panel/api/accounts/unpin_all", p.withAuth(p.accountUnpinAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("GET /panel/api/reqlog", p.withAuth(p.reqLog))
	p.mux.HandleFunc("GET /panel/api/model-locks", p.withAuth(p.modelLocks))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("GET /panel/api/update/check", p.withAuth(p.handleUpdateCheck))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	p.mux.HandleFunc("POST /panel/api/system/restart", p.withAuth(p.systemRestart))
	p.mux.HandleFunc("POST /panel/api/system/shutdown", p.withAuth(p.systemShutdown))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		primary, previous := p.apiKeys()
		ok, usedPrev := httpauth.VerifyBearerTransition(r, primary, previous)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		if usedPrev {
			httpauth.WarnPreviousKeyUse("panel")
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	primary, _ := p.apiKeys()
	return primary
}

// apiKeys 返回 (主密钥, 过渡密钥)：Live 优先，回落静态字段。
func (p *Panel) apiKeys() (string, string) {
	if p.cfg.Live != nil {
		s := p.cfg.Live.Load()
		return s.APIKey, s.APIKeyPrevious
	}
	return p.cfg.APIKey, p.cfg.APIKeyPrevious
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息 + 积分用量与路由模式汇总。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	accounts := p.cfg.Pool.List()
	var (
		totalCredits          int64
		totalCreditsCap       int64
		totalCreditsUsed      float64
		totalDailyCreditsUsed float64
		urgent48hCredits      int64
		urgent72hCredits      int64
	)
	for _, a := range accounts {
		totalCredits += a.Credits
		totalCreditsCap += a.CreditsTotal
		totalCreditsUsed += a.CreditsUsed
		totalDailyCreditsUsed += a.DailyCreditsUsed
		urgent48hCredits += a.Urgent48hRemain
		urgent72hCredits += a.Urgent72hRemain
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"pinned_uids":     p.cfg.Pool.PinnedUIDs(),
		"enterprise_accounts": func() int {
			n := 0
			if p.cfg.Pool != nil {
				for _, st := range p.cfg.Pool.List() {
					if st.IsEnterprise() {
						n++
					}
				}
			}
			return n
		}(),
		"total_credits":            totalCredits,
		"total_credits_cap":        totalCreditsCap,
		"total_credits_used":       totalCreditsUsed,
		"total_daily_credits_used": totalDailyCreditsUsed,
		"urgent_48h_credits":       urgent48hCredits,
		"urgent_72h_credits":       urgent72hCredits,
		"accounts":                 accounts,
	})
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	// 缓存命中直接返回（10 分钟内不重复打上游）；?refresh=1 穿透。
	// 每次未命中要打 2~3 次 /v3/config（CN 双 UA + global），面板切页就刷一轮，
	// 既慢又把网关伪装成扫描器（2026-10-21）。
	//
	// 【stale-while-revalidate，2026-10-21 生产事故后补】
	// 只有 TTL 判定时，缓存一过期就必须**同步**打上游；上游 TLS 握手一抖动
	// （实测 `net/http: TLS handshake timeout`），这一页会卡死 30s+，客户端直接超时，
	// 而同机的 overview/packages 仍是毫秒级 —— 一个只读展示页不该被上游抖动拖死。
	// 现在：新鲜 ⇒ 直接回；过期但有旧数据 ⇒ **立刻回旧数据**（stale=true）并后台刷新；
	// 完全没缓存 ⇒ 才同步拉（首次必然如此）。
	if !wantRefresh(r) {
		if e, ok := p.cachedModels("all", now); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "models": e.entries, "cached": true,
				"age_seconds": int(now.Sub(e.fetched).Seconds()),
				"warnings":    e.errs,
			})
			return
		}
		if e, ok := p.staleModels("all"); ok && len(e.entries) > 0 {
			p.refreshModelsAsync()
			warns := append([]string{}, e.errs...)
			warns = append(warns, "目录已过期，正在后台刷新（当前展示上次结果）")
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "models": e.entries, "cached": true, "stale": true,
				"age_seconds": int(now.Sub(e.fetched).Seconds()),
				"warnings":    warns,
			})
			return
		}
	}

	out, fetchErrs := p.fetchModels()

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	p.storeModels("all", out, fetchErrs, now)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out, "cached": false, "warnings": fetchErrs})
}

// fetchModels 从上游拉取双域模型目录（不写缓存、不写响应）。
// 抽出来的目的：同步路径（首次无缓存）与后台刷新路径共用同一份探测逻辑。
func (p *Panel) fetchModels() (out []map[string]any, errs []string) {
	out = make([]map[string]any, 0)

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				errs = append(errs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					errs = append(errs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}
	return out, errs
}

// refreshModelsAsync 后台刷新模型目录（stale 返回路径专用）。
// 用 beginRefresh 去重：同一时刻只允许一个刷新在跑，避免多个面板请求同时打上游。
func (p *Panel) refreshModelsAsync() {
	if !p.beginRefresh("models") {
		return
	}
	gosafe.Do("panel models background refresh", func() {
		defer p.endRefresh("models")
		out, errs := p.fetchModels()
		if len(out) > 0 {
			p.storeModels("all", out, errs, time.Now())
		}
	})
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPin 强制指定路由账户（仅保存在内存中，重启后重置）。
func (p *Panel) accountPin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	realm, err := p.cfg.Pool.PinAccount(uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 强制指定路由账户 uid=%s (realm=%s)", uid, realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"realm":   realm,
		"message": "已强制指定路由账户: " + uid + "（重启后重置）",
	})
}

// accountUnpin 取消强制指定路由账户。
func (p *Panel) accountUnpin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if err := p.cfg.Pool.UnpinAccount(uid); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 取消强制指定路由账户 uid=%s", uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     uid,
		"message": "已恢复自动智能多梯队调度",
	})
}

// accountUnpinAll 清除所有域的强制指定。
func (p *Panel) accountUnpinAll(w http.ResponseWriter, r *http.Request) {
	if err := p.cfg.Pool.UnpinAccount("all"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 已清除所有域的强制路由指定")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "已恢复全局自动智能多梯队调度",
	})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// 与排程共用一把"签到中"锁（第十轮审计 F5）：单号签到此前直接打上游，既无账号锁
	// 也无 checkinMu —— 与定时 CheckinAll 并发时，同一账号会被两次 DailyCheckin 打，
	// 且后到的 NoteCheckin 会用本次结果覆盖当天状态（网络失败即把已成功写成 fail）。
	if p.cfg.TryCheckinLock != nil && !p.cfg.TryCheckinLock() {
		writeErr(w, http.StatusConflict, "已有一次签到在跑，请稍后重试")
		return
	}
	if p.cfg.UnlockCheckin != nil {
		defer p.cfg.UnlockCheckin()
	}
	checkinMsg := ""
	ckStatus := "ok"
	if a.IsGlobal() {
		if err := p.cfg.Upstream.DailyChatGlobal(a); err != nil {
			checkinMsg = err.Error()
			ckStatus = "fail"
		} else {
			// 与排程路径共用同一持久化标记（第十一轮审计 F6）：否则手动点过一次后，
			// 当天排程还会再打一次同一账号的每日活跃。
			p.cfg.Pool.NoteDailyMark(uid, "daily_chat", cstime.Today())
			checkinMsg = "国际版每日活跃打卡完成（获取每日 30~50 积分奖励）"
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				checkinMsg = "今天已签到" // 幂等成功，不当作失败
				ckStatus = "already"
			} else {
				checkinMsg = err.Error() // 业务错误照常查余额
				ckStatus = "fail"
			}
		}
	}
	p.cfg.Pool.NoteCheckin(uid, ckStatus)
	resp := map[string]any{"ok": true}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：UserResource → SetCredits（不触碰冷却状态）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, err := p.cfg.Upstream.UserResource(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCredits(uid, remain, total)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go gosafe.Do("panel checkin all", func() { p.cfg.Scheduler.RunCheckinNow(true) })
	log.Printf("panel: 手动全量签到已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go gosafe.Do("panel travel all", func() { p.cfg.Scheduler.RunTravelNow(true) })
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go gosafe.Do("panel activity all", func() { p.cfg.Scheduler.RunActivityNow(true) })
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go gosafe.Do("panel keepalive all", func() { p.cfg.Scheduler.RunKeepaliveNow(true) })
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。hours 查询参数控制统计窗口（默认 72，上限 1440=60
// 天）：卡片汇总/按域/按账号/按模型/时序**全部**按该窗口统计。显式 hours=0 表示
// 全部历史（含 90 天前折叠出的日桶，看长期趋势）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	snap := p.cfg.Usage.Snapshot(hours, nicks)
	if p.cfg.Pool != nil {
		accounts := p.cfg.Pool.List()
		accMap := make(map[string]pool.Status)
		var totalDailyCreditsUsed, totalCreditsUsed float64
		for _, s := range accounts {
			accMap[s.UID] = s
			totalDailyCreditsUsed += s.DailyCreditsUsed
			totalCreditsUsed += s.CreditsUsed
		}
		snap.TotalDailyCreditsUsed = totalDailyCreditsUsed
		snap.TotalCreditsUsed = totalCreditsUsed
		for i := range snap.ByAccount {
			if st, ok := accMap[snap.ByAccount[i].Key]; ok {
				snap.ByAccount[i].DailyCreditsUsed = st.DailyCreditsUsed
				snap.ByAccount[i].TotalCreditsUsed = st.CreditsUsed
			}
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	// D1：5 分钟缓存（按账号打上游计费端点，实测 ~1.0s / 5 次请求）。
	// ?refresh=1 强制穿透；返回 cached/age_seconds 供前端提示。
	now := time.Now()
	if !wantRefresh(r) {
		if payload, ok := p.cachedPackages(now); ok {
			writeJSON(w, http.StatusOK, payload)
			return
		}
		// 同 models：过期但有旧数据 ⇒ 先回旧数据 + 后台刷新，不把用户卡在上游抖动上
		//（这个接口要按账号逐个打计费端点，抖动时更容易卡）。
		if payload, ok := p.stalePackages(); ok {
			if p.beginRefresh("packages") {
				gosafe.Do("panel packages background refresh", func() {
					defer p.endRefresh("packages")
					p.fetchAndStorePackages()
				})
			}
			out := map[string]any{}
			for k, v := range payload {
				out[k] = v
			}
			out["stale"] = true
			writeJSON(w, http.StatusOK, out)
			return
		}
	}

	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go gosafe.Do("panel credit packages", func() {
			// i / s 由 for 循环逐轮独立（go.mod go1.22）提供，闭包直接捕获即可。
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		})
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	payload := map[string]any{"accounts": out, "cached": false}
	// 只有全部账号都取到数据才缓存：把一次部分失败的结果固化 5 分钟会让页面一直显示缺数据。
	allOK := true
	for _, it := range out {
		if it.Error != "" {
			allOK = false
			break
		}
	}
	if allOK {
		cachePayload := map[string]any{"accounts": out, "cached": true}
		p.storePackages(cachePayload, now)
	}
	writeJSON(w, http.StatusOK, payload)
}

// fetchAndStorePackages 后台刷新积分构成（stale 返回路径专用）。
// 只在全部账号都取到数据时才落缓存，语义与同步路径一致。
func (p *Panel) fetchAndStorePackages() {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for i, st := range accts {
		wg.Add(1)
		go gosafe.Do("panel credit packages bg", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
			a := p.cfg.Pool.AuthByUID(st.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		})
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	for _, it := range out {
		if it.Error != "" {
			return // 部分失败不缓存
		}
	}
	p.storePackages(map[string]any{"accounts": out, "cached": true}, time.Now())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// accountPause 暂停选号（上游 fd835df 移植）：账号退出选号候选，但**照常**参与保号任务
// （签到 / 活跃上报 / 保活 / 余额刷新 / 旅行 / 成长任务），唯一跳过夜猫子真实对话。
// 与 accountDisable 正交：不碰冷却/熔断/模型级冷却，可随时 resume。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 恢复选号（幂等）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// modelLocks 面板「模型锁池」视图数据（只读；上游 b3f92dd 的本地化实现）。
func (p *Panel) modelLocks(w http.ResponseWriter, r *http.Request) {
	rows := p.cfg.Pool.ModelLocks()
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}
