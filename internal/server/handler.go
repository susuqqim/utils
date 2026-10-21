// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/httpauth"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// 【P1-7】RestrictedRotateMax 收到 11140（账号画像风控）后还允许跨号重试的次数。
	// 0（默认）= 收到即停手：同一请求继续换号会被上游视为同源批量试探，
	// 实测 6 分钟内把 3 个账号逐个连坐限制。
	RestrictedRotateMax int
	// RestrictionTTL 11140 限制的持久化时长（默认 2h）；到期后账号自动回到可选池。
	RestrictionTTL time.Duration
	// RestrictionWaveWindow / RestrictionWaveMin 整池波次暂停：同一 realm 在窗口内
	// 被限制 ≥Min 个账号 ⇒ 暂停该 realm 一段时间，避免继续试探连坐。
	RestrictionWaveWindow time.Duration
	RestrictionWaveMin    int
	// RestrictionPause 波次暂停时长（默认 = 窗口）。
	RestrictionPause time.Duration
	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler
	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder
	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// HiddenModels 隐藏的模型名（bare 或带 cn:/global: 前缀皆可；默认空 = 零回归）。
	// 语义：/v1/models 不列出 + 直接请求**本地 400 拒绝**（不打上游、不计费、不写冷却）。
	HiddenModels []string
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:       h.cfg.APIKey,
		SoftCooldown: h.cfg.SoftCooldown,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	hidden map[string]struct{} // 由 Config.HiddenModels 构建（见 hidden_models.go）
	cfg    Config
	mux    *http.ServeMux

	// 【P1-7】上游风控限制的波次记账：realm → 最近的 11140 时刻；以及整池暂停截止时间。
	restrictMu       sync.Mutex
	restrictHits     map[string][]time.Time
	realmPausedUntil map[string]time.Time
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	// 【P1-7】上游风控限制相关默认值：2h 限制、15min 窗口 / 2 个号触发、暂停 15min。
	if cfg.RestrictionTTL <= 0 {
		cfg.RestrictionTTL = 2 * time.Hour
	}
	if cfg.RestrictionWaveWindow <= 0 {
		cfg.RestrictionWaveWindow = 15 * time.Minute
	}
	if cfg.RestrictionWaveMin <= 0 {
		cfg.RestrictionWaveMin = 2
	}
	if cfg.RestrictionPause <= 0 {
		cfg.RestrictionPause = cfg.RestrictionWaveWindow
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), hidden: hiddenSet(cfg.HiddenModels)}
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS：api_key 非空时维持原有的宽松口径（`*`）——密钥本身即边界，且下游 Web 类
	// 客户端常需要跨域直连本机网关。**api_key 为空时网关不鉴权**，此时若仍回 `*`，
	// 用户浏览器里打开的任意网页都能驱动本机网关（烧积分、读 /status 账号画像）；
	// 故只在无密钥时收紧：仅"Host 指向本机回环 + 同源"的请求放行（见 keylessRequestAllowed，
	// 判据以 Host 为主，防 DNS rebinding）。
	if h.loadLive().APIKey == "" && !keylessRequestAllowed(r) {
		log.Printf("WARN: [server] 拒绝非本机来源 Host=%q Origin=%q（api_key 为空时不鉴权的兜底）；如需跨机/跨域访问请先设置 api_key",
			r.Host, r.Header.Get("Origin"))
		writeOpenAIError(w, http.StatusForbidden, "cross_origin_denied",
			"gateway has no api_key configured; only loopback (127.0.0.1/localhost) same-origin access is allowed — set api_key to allow other origins")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Pool 是 NewHandler 的必需依赖（main.go 恒传入），chat/models/status/healthz
	// 四条路由都会读它。此处统一兜底，把"依赖缺失"这种**编程错误**变成一条可诊断的
	// 500，而不是让请求在选号/统计阶段的 nil 解引用里 panic——panic 会打断连接且
	// 堆栈指向 pick/state 内部，极难定位到"构造 handler 时漏了 Pool"。
	if h.cfg.Pool == nil {
		log.Printf("ERR: [server] %s %s: pool 未配置（NewHandler 缺少 Pool 依赖）", r.Method, r.URL.Path)
		writeOpenAIError(w, http.StatusInternalServerError, "server_misconfigured", "网关账号池未初始化")
		return
	}
	// 同上：Upstream 也是必需依赖。缺它时 /v1/models 会在**上游探测 goroutine**里
	// nil 解引用 —— goroutine 的 panic 不会被 net/http 的 handler recover 兜住，
	// 结果不是 500 而是**整个网关进程退出**。这里统一变成可诊断的 500。
	//
	// 作用范围**只限真正要打上游的端点**：/healthz 是存活探针（Docker HEALTHCHECK、
	// 托盘轮询），不依赖上游客户端——把它也变成 500 会让"网关活着但依赖装错"被误判为
	// 进程死亡（编排层会重启/摘流量）。
	if h.cfg.Upstream == nil && needsUpstream(r.URL.Path) {
		log.Printf("ERR: [server] %s %s: upstream 未配置（NewHandler 缺少 Upstream 依赖）", r.Method, r.URL.Path)
		writeOpenAIError(w, http.StatusInternalServerError, "server_misconfigured", "网关上游客户端未初始化")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		live := h.loadLive()
		ok, usedPrev := httpauth.VerifyBearerTransition(r, live.APIKey, live.APIKeyPrevious)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		if usedPrev {
			httpauth.WarnPreviousKeyUse("server")
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

// enterpriseAccounts 企业号只读计数（S5 第 1 步）：判定走 pool.Status.IsEnterprise()
// （只认非空 enterpriseId；空白/缺失判个人号），与门控点、面板标记同一口径。
func (h *Handler) enterpriseAccounts() int {
	n := 0
	if h.cfg.Pool == nil {
		return 0
	}
	for _, st := range h.cfg.Pool.List() {
		if st.IsEnterprise() {
			n++
		}
	}
	return n
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	dailyCredits, totalCredits, dailyTokens, totalTokens := h.cfg.Pool.UsageTotals()
	// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
	// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
	//
	// paused 单独计数：disabled 口径含 paused 是上游语义，但运维需要区分"禁用"与
	// "暂停选号"（追加键，不动既有键 —— 审查 B 的 P1-2）。
	pausedCount := 0
	for _, st := range h.cfg.Pool.List() {
		if st.Paused {
			pausedCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":            h.cfg.Pool.List(),
		"total":               total,
		"healthy":             healthy,
		"cooling":             cooling,
		"disabled":            disabled, // 口径含 paused（上游语义）；细粒度看下方 paused
		"paused":              pausedCount,
		"enterprise_accounts": h.enterpriseAccounts(),
		"in_flight_full":      inFlightFull,
		"daily_credits_used":  dailyCredits,
		"total_credits_used":  totalCredits,
		"daily_tokens_used":   dailyTokens,
		"total_tokens_used":   totalTokens,
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		// 上游静默截断（有有效帧但没发 [DONE] 就 EOF）的累计次数：只读观测项，
		// 便于发现"上游悄悄少了收尾帧"而不必翻日志（该形态按正常结束兜底补 [DONE]，
		// 语义不变，见 upstream.StreamNoDoneCount）。
		"stream_no_done_total": upstream.StreamNoDoneCount(),
		"redis_mode":           redisMode,
		"credit_floor":         h.cfg.Pool.CreditFloor(),
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// 静态 CN 模型表（api-reference §5，**动态拉取失败才用的兜底**，见 modelList）：
// 动态接口成功时以动态结果优先（含真实 context_length/max_output_tokens）；
// 本表全用 context_length=131072 只是兜底形态，不得理解为权威值。
// 触底前提：fetchDynamicModels 返回空（拉取失败 / 负缓存 / 池中无健康 CN 号）。
// staticModels 静态兜底模型表：仅在动态清单（双 UA 并集）拉取失败时使用。
// 2026-10-03 按上游实测补齐了权威清单（/mnt/d/kk/agent配置/workbuddy_models_manifest.json）
// 里的在用模型 —— 此前静态表只有 10 个 cn 模型，一旦上游目录接口故障，/v1/models 会少列
// deepseek 系列等在用模型，而客户端配置正是按这份清单生成的。
// 2026-10-03 同时移除了实测已不在上游目录的 hy3-preview / hy3-preview-agent（兜底期不该
// 列出用不了的模型；依据 = 测试实例 17964 的实时 /v1/models 清单）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 48000},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 64000},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 32000},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 524288},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 64000},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 393216},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072, "max_output_tokens": 50000},
	{"id": "hy4-preview-f", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 64000},
	{"id": "space-bunny", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 128000},
	{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 393216},
	{"id": "auto", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000, "max_output_tokens": 32000},
	{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 131072},
	{"id": "hy3-x", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 192000, "max_output_tokens": 64000},
	{"id": "kimi-k3-1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 1048576},
	{"id": "kimi-k2.8-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 131072},
	{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 64000},
	{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000, "max_output_tokens": 131072},
	{"id": "kimi-k2.6", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 256000, "max_output_tokens": 32000},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长。缩到 10min 避免长时间与上游新增模型脱节。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		// 隐藏"有名无服务"的模型（见 hidden_models.go）：客户端枚举不到就不会再逐个试。
		"data": filterHidden(h.modelList(), func(m map[string]any) string { id, _ := m["id"].(string); return id }, h.hidden),
	})
}

// globalModelsHint 仅用于 `/v1/models` 的容量预估（**不是**兜底名单）。
//
// 2026-10-03 起 global 名单是纯动态的（见 fetchGlobalModels）：没有可用 global 账号时返回空，
// 绝不再回落 `upstream.GlobalModelNames` —— 那是 21 个静态名字，纯 CN 池会据此对外宣称
// 21 个必然失败的 global 模型（审查 F1）。
const globalModelsHint = 32

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 动态失败回退静态表；global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0, (len(staticModels)+globalModelsHint)*2)
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		for _, mi := range infos {
			cl := mi.ContextWindow
			if cl == 0 {
				cl = 131072
			}
			out = append(out, map[string]any{
				"id":                "cn:" + mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    cl,
				"max_output_tokens": mi.MaxTokens,
			})
			out = append(out, map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    cl,
				"max_output_tokens": mi.MaxTokens,
			})
		}
	} else {
		for _, m := range staticModels {
			e1 := make(map[string]any, len(m)+1)
			for k, v := range m {
				e1[k] = v
			}
			if id, ok := m["id"].(string); ok {
				e1["id"] = "cn:" + id
			}
			out = append(out, e1)

			e2 := make(map[string]any, len(m))
			for k, v := range m {
				e2[k] = v
			}
			out = append(out, e2)
		}
	}
	if h.cfg.GlobalEnabled {
		for _, id := range h.fetchGlobalModels() {
			// **只发 global:<id>，不再同时发裸名**（2026-10-03 修）。
			//
			// 命名协议（AGENTS.md §1）：裸名 → realm 缺省为 **cn**。因此把 **global 独有**模型
			// 也以裸名列出，等于对外发了一张"必然失败"的票：客户端照单选裸名 → 出站走 cn →
			// 上游 400/11102（实测这类条目有 18 个：o4-mini、gpt-5.6-sol、gemini-3.5-flash、
			// primary-model …）。
			// 两域共有（9 个）的裸名不受影响：cn 分支已经发过裸名 + cn: 前缀，客户端照裸名调用
			// 走 cn 是**正确**的路由。
			out = append(out, map[string]any{
				"id":             "global:" + id,
				"object":         "model",
				"created":        1753600000,
				"owned_by":       "workbuddy",
				"context_length": 131072,
			})
		}
	}
	return dedupeModelIDs(out)
}

// dedupeModelIDs 按 id 去重（保留首次出现的条目）。
//
// 为什么需要：跨 realm 同名模型会同时产出 `cn:X` / `global:X` / 裸 `X`，其中**裸名两份**
// （cn 分支一份、global 分支一份）→ 严格按 OpenAI 协议消费 `/v1/models` 的客户端会看到重复 id
// （2026-10-03 实测 9 个模型重复）。这是既有行为、非某次改动引入，但顺手修掉更干净：
// 裸名保留第一份（语义与路由一致：裸名按 cn 解析，见模型命名协议）。
func dedupeModelIDs(in []map[string]any) []map[string]any {
	seen := make(map[string]bool, len(in))
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		id, _ := m["id"].(string)
		if id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		out = append(out, m)
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（**纯动态**）。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无可用 global 账号 → 返回 **nil**（不列任何 global 模型）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 为什么不再回落静态名单（2026-10-03 修，审查 F1）：`upstream/global_models.go` 的包注释
// 写得很清楚——"纯动态：不再回落任何静态名单……**假名单只会让客户端选到 11102 的模型**
// （产品决策：无兜底）"。而这里原先在"没有 global 账号"时 `return globalModels`（历史静态
// 21 名），于是一个纯 CN 池会对外宣称 21 个必然失败的 global 模型（实测：池内只有 CN 号时
// `/v1/models` 列出 21 条 `global:`，实调任一 → 503 no_healthy_account）。客户端按这份清单
// 生成配置就会得到 21 个必失败条目。
//
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() []string {
	uids := h.cfg.Pool.AvailableUIDsForRealm("global")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct)
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
// 只从 CN realm 账号拉取（PickExcludingForRealm(nil,"","cn")）：全局账号的模型列表
// 未必与 CN 一致，动态模型表只服务 CN 前缀（global 用内置静态名单）。
// modelsFetchTimeout 限制「拉取动态模型目录」的最长等待时间。
// 2026-10 缺陷：FetchModels 内部是 chan receive，网络卡住时会**无界阻塞** ⇒ /v1/models
// 与面板一起挂死（测试 TestModelsEndpoint 曾在 TLS 握手上挂到超时）。这里改为有界等待，
// 超时按失败处理（走既有 5min 负缓存 + 静态表回退）。var 而非 const，便于测试覆写。
var modelsFetchTimeout = 8 * time.Second

// fetchModelsBounded 在 modelsFetchTimeout 内等待 fn；超时返回 error（不阻塞调用方）。
// 通道带缓冲，超时后 goroutine 仍能写入并退出，不会泄漏阻塞。
func fetchModelsBounded(acct *auth.Auth, fn func(*auth.Auth) ([]upstream.ModelInfo, error)) ([]upstream.ModelInfo, error) {
	type res struct {
		infos []upstream.ModelInfo
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		i, e := fn(acct)
		ch <- res{i, e}
	}()
	select {
	case r := <-ch:
		return r.infos, r.err
	case <-time.After(modelsFetchTimeout):
		return nil, fmt.Errorf("fetch models: timeout after %v", modelsFetchTimeout)
	}
}

func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := fetchModelsBounded(acct, h.cfg.Upstream.FetchModels)
	if err != nil || len(infos) == 0 {
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持全局负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// extractHeaderKey 从 HTTP 请求头提取会话键（支持标准与常见客户端反代头）。
func extractHeaderKey(r *http.Request) string {
	for _, h := range []string{"X-Conversation-ID", "X-Session-ID", "Session-Id", "X-User-ID"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	// 注：Pool 非空已由 ServeHTTP 统一保证（nil 时在此前返回 500）。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（超过上限一半）：小探针（{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	//
	// 落盘位置（2026-10-03 修）：原先硬编码 `/app/data/last_request.json`（Docker 遗留）→
	// 原生形态下该目录不存在，**每次都写失败并打一条 ERR**（开关开着时刷屏）。现在：
	//   - 值里含路径分隔符 → 当作完整路径用（用户可指定任意位置）；
	//   - 否则落到 `data/last_request.json`（相对进程工作目录 = 配置文件所在目录，见 §O；
	//     Docker 里 cwd=/app → /app/data/... 与旧行为等效）。
	if v := os.Getenv("WB2A_DUMP_REQ"); v != "" && len(body)*2 >= int(limit) {
		dumpReqDump(v, body)
	}
	var peek struct {
		Stream   bool            `json:"stream"`
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
	}
	// 【F1，2026-10-21】本地最小校验：把"注定被上游拒绝"的请求在本地 400 掉。
	//
	// 此前这里是 `_ = json.Unmarshal(body, &peek)` —— 解析错误被**静默忽略**，于是
	// 缺 messages / 空 messages / 缺 model 的请求会**真的发到上游**（实测 1.2~1.8s），
	// 拿回 11101/11133 再转成 invalid_request_error。每一次都是白烧一次上游调用，
	// 既浪费账号配额，又在风控眼里多出一条异常请求（本项目最在意的风险）。
	//
	// 校验只覆盖 OpenAI 规范**明确要求**的字段，不做更严的语义检查，
	// 以免误伤上游实际能容忍的灵活形态。
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json",
			"请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(peek.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_model",
			"缺少 model 字段：请在请求体里指定模型名（可用清单见 /v1/models）")
		return
	}
	if len(peek.Messages) == 0 || string(peek.Messages) == "null" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_messages",
			"缺少 messages 字段：OpenAI Chat Completions 要求 messages 为非空数组")
		return
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(peek.Messages, &msgs); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_messages",
			"messages 必须是数组: "+err.Error())
		return
	}
	if len(msgs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "empty_messages",
			"messages 不能为空数组：至少需要一条消息")
		return
	}

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本（见
	// upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」与
	// 「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)
	// 隐藏模型直接本地拒绝：不打上游、不计费、**不写任何冷却**（见 hidden_models.go）。
	if h.isHidden(bareModel) {
		writeOpenAIError(w, http.StatusBadRequest, "model_hidden", fmt.Sprintf("model %q 在上游已无服务（service info not found），已被网关隐藏；请改用 /v1/models 列表中的模型", bareModel))
		return
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	// 轮转期间的失败分类聚合。终态状态码必须由**全部**失败的性质决定，而不是"最后一次"
	// （选号是加权随机，混合失败下同一请求重发可能 400 也可能 503，SDK 行为不可预测）：
	//   - sawModelBlocked：模型在**已试账号**的后端不可用；是否升级为 400 还要看
	//     "该 realm 的候选是否都试过了"（见 modelUnavailableFinal）。
	// 其余失败（429/熔断/账号故障/传输错误/池空）不影响这两个标志，终态走 503（可重试）。
	//   - sawBadParams：任一次命中即判 400 —— "参数/上下文被拒"是请求自身的属性。注意
	//     **请求体解析失败（11101）在循环内即刻 400 终止**，不走这里；走到这里的只有
	//     模型相关的参数类（11133 等），它们可能换号成功，故仍需终态聚合。
	var sawBadParams, sawModelBlocked bool
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：从请求体提取会话键（显式 conversation_id/session_id 优先，其次 HTTP 请求头，
	// 兜底使用首条 user 消息指纹 ConversationFingerprint）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型仍可用），
	// 必须重分配——否则会被钉在这个号上反复失败。
	sessKey := session.ExtractKey(body)
	headerKey := extractHeaderKey(r)
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = headerKey
	}
	if stickyKey == "" {
		stickyKey = session.ConversationFingerprint(body)
	}

	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级兜底聚合键：无显式会话键客户端（OpenAI 兼容协议）按 body 里最后一条 user 消息
	// 派生轮级键（同轮内所有上游调用同键）。必须在下方 prompt.Rewrite / rewriteModel 之前取。
	turnKey := ""
	if sessKey == "" && headerKey == "" {
		turnKey = session.TurnKey(body)
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// ok 表示"该次上游尝试是否成功"（决定面板的失败率与成功率），与"是否带 usage"解耦：
	// 旧实现把 ok 直接传成 delta.Has*（有没有 token），于是
	//   - 成功但上游没给 usage 的请求被记成失败（假失败）；
	//   - 中途截断但仍收到部分 usage 的请求被记成成功（假成功，面板成功率虚高）。
	// canceled 表示客户端取消（499）：计请求数、不计失败（账号无过错）。
	// sawUpstreamErr 由流式路径在拿到 stats 后回填（见下）；非流式保持 nil。
	var sawUpstreamErr func() bool
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, started time.Time, ok bool, canceled bool, ttfb time.Duration) {
		if ok && (sawUpstreamErr == nil || !sawUpstreamErr()) {
			// 该 (账号, 模型) 实测又通了：清掉 11102 负缓存（只清 11102 条目，不碰 6004 冷却表）。
			// 但 HTTP 200 + error 帧（6004/审核等，sse.go 会原样透传并计有效帧）不算"通了"，
			// 否则会把刚写下的 11102 负缓存误清（审查 D 的 P2-8）。
			h.cfg.Pool.BlockModelClear(uid, bareModel)
		}
		isCanceled := canceled
		delta.Model = peek.Model
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs
		// 速率口径 = 生成期（总时长 - 首帧等待），与日志行同源同口径（见 logging.go 的
		// tokensPerSecond 注释：端到端口径会把提示词处理/排队算进分母，短回答被低估数倍，
		// 导致面板数字与 DSH 等客户端对不上）。非流式/未测到首帧时 ttfb=0 → 回退端到端。
		if delta.HasCompletionTokens && delta.CompletionTokens >= 0 && latency > 0 {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = tokensPerSecond(int(delta.CompletionTokens), latency, ttfb)
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				CachedTokens:     delta.CachedTokens,
				HasCached:        delta.HasCachedTokens,
				Credit:           delta.Credit,
				HasCredit:        delta.HasCredit,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
				Canceled:         isCanceled,
			}, ok)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough：透传客户端原始 system（不改写）。
	// 注意：跨请求全局"降级状态机"已彻底移除（只保留本请求内的一次 Degraded 重试），
	// 绝不在入口强制降级所有 Agent 提示词。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造，见
	//     ResolveConversationID；官方后台不校验一致，空会话则不建立聚合键）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准），否则按粘性 key 进程内稳定生成（同会话
	//     恒同值）；粘性 key 也为空时走轮级兜底（session.TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级 NewMessageID——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	convID := resolveCacheConvID(body, r.Header, sessKey)
	chatMeta := upstream.ChatMeta{ConversationID: convID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else if headerKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(headerKey)
	} else {
		// 无会话键客户端：轮级兜底——同轮内 tool call 多轮 / 换号重试 / 降级重发
		// 共享同键，用户发下一条消息自动换键。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：Phase 3 主刷分块与临期加权调度（已建立会话也服从主刷分块与临期加权）。
		// 若启用了分块调度（ChunkQuotaCredits > 0），由 Pool 主导分块与切号；
		// 当未启用分块调度（ChunkQuotaCredits <= 0）时，遵从会话粘性号优先。
		var acct *auth.Auth
		if h.cfg.Pool.PinnedUIDForRealm(realm) == "" && h.cfg.Pool.SchedulePolicy().ChunkQuotaCredits <= 0 && stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：请求携带 model 时启用 6004 模型级冷却豁免，
			// realm 谓词过滤跨域账号（PickExcludingForModel 内部 model 空即退化 PickExcluding）。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			modelBlock = h.cfg.Pool.ModelBlocked(bareModel, realm)
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 【P1-7】整池波次暂停：同域窗口内已有 N 个账号被上游限制 ⇒ 本域整体停手一段时间，
		// 避免继续试探把剩余账号也拖下水（实测连坐是"一个请求遍历整池"放大出来的）。
		if h.realmPaused(acct.Realm()) {
			st.status = http.StatusServiceUnavailable
			lastErr = fmt.Errorf("realm %s paused after upstream account restrictions", acct.Realm())
			log.Printf("WARN: [server] realm=%s 处于风控波次暂停中，本请求不再向该域发起上游调用", acct.Realm())
			break
		}

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		if terr != nil {
			if errors.Is(terr, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				// 客户端主动取消（如在收到头之前点击"停止"）：不罚号、不轮转、立即退出；
				// 用量侧记为取消（计请求数、不计失败）。
				// 口径说明：本路径（连响应头都没拿到）**不清** softStreak/成功计数——账号压根
				// 没开始服务，没有任何"它现在是好的"的证据；而流式与非流式两条"已开始服务"
				// 的 499 路径会调 NoteClientAbort（清计数、不记业绩）。
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, true, 0)
				st.status = 499
				log.Printf("[server] chat_stream uid=%s: client cancelled before response headers, upstream aborted", logfmt.UID8(acct.UID))
				return
			}
			// 上游超时 / 停滞：**不换号、不罚号**（上游 55d1e4d）。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，只会把客户端
			// 拖到 MaxRotate × header_timeout（部署值 600s 时最坏约半小时），期间还给一串健康号
			// 喂连败计数。此前全仓没有任何超时识别，超时和"网络抖动"共用同一条换号路径。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, false, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout uid=%s: %v (rotation stopped, account not penalized)", logfmt.UID8(acct.UID), terr)
				break
			}
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, false, 0)
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, false, 0)
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				// 单次请求内尝试一次 Degraded 中性提示词重试（防指纹误报）。
				// 隔离在当前单次请求生命周期内，绝不触发全局跨请求降级，确保其他会话与后续请求的系统提示词完整性。
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> isolated degraded prompt retry for this request")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**、不暴露账号/冷却/上游错误码
				// （此前会落到 503 no_healthy_account + lastErr 泄露 11128 与账号语义）。
				// 不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError），但 content_blocked
				// 是本请求的终态——换任何账号都会撞同一审核，轮转纯属浪费时间。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				msg := upstream.ContentBlockedClientMessage(string(respBody))
				writeOpenAIError(w, http.StatusBadRequest, "content_blocked", msg)
				st.status = http.StatusBadRequest
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				writeOpenAIError(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)))
				st.status = http.StatusBadRequest
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIError(w, http.StatusBadRequest, "image_invalid", msg)
				st.status = http.StatusBadRequest
				return
			}
			// 参数/schema 级 400（如 11129 invalid_function_call_parameters）：同一 body 换任何
			// 账号都会被同样拒收，立即透传上游原文，避免把健康号配额烧在必然失败的轮转上。
			if kind == upstream.ErrClient && upstream.IsParameterSchemaError(string(respBody)) {
				fail(acct.UID)
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", string(respBody))
				st.status = http.StatusBadRequest
				return
			}
			// 请求体解析失败（11101/11133）：与 11115 / 11729 同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会把无效请求放大成 N 次上游调用 + 占用在途名额，
			// 末端还会落到「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试（上游 fd5c5a6 / issue #102 同族修法）。
			// 只对**请求体解析失败**（11101 / Unmarshal chat params failed）快速失败：
			// 同一字符串在任何账号上都是同样的解析结果，轮转只会放大无效请求并在末端伪装成
			// "账号不可用"（上游 fd5c5a6）。
			//
			// **不覆盖**其余参数/上下文类标记（11133 model_param_invalid、too many tokens、
			// context length、max_tokens must be…）：本项目自己的证据表明它们是**模型/后端
			// 相关**的 —— 同一份 body 在 hy4 上通过、在 GPT 系 15 被拒 16 通过（见
			// clampGPTMinMaxTokens），换号可能成功 ⇒ 必须保留轮转，并在末端以 sawBadParams
			// 归 400。审查 A 的 P1-2 正是这一点。
			if kind == upstream.ErrBadParams && upstream.IsRequestParseFailure(string(respBody)) {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				// 错误 code 沿用本项目既有契约（§R.8：400 + invalid_request_error），
				// 只取上游「不轮转、立即透传」的语义——换号必然同样失败。
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
				st.status = http.StatusBadRequest
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			if kind == upstream.ErrModelBlocked {
				sawModelBlocked = true
			}
			if kind == upstream.ErrBadParams {
				// 模型/后端相关的参数拒绝（11133 等）：轮转可能命中能接的账号，
				// 末端按 sawBadParams 归 400（恢复 fast-fail 之前的终态语义）。
				sawBadParams = true
			}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
			fail(acct.UID)
			// 【P1-7】11140 账号画像风控：默认**不跨号重试**。同一请求继续换号会被上游
			// 视为同源批量试探，把整池账号逐个连坐（实测 6 分钟内 3 个号连续被限制）。
			if kind == upstream.ErrAccountFault && upstream.IsAccountRestricted(string(respBody)) {
				h.noteRestriction(acct)
				if h.cfg.RestrictedRotateMax <= 0 {
					st.status = http.StatusForbidden
					writeOpenAIError(w, http.StatusForbidden, "account_restricted",
						"上游判定该账号存在异常使用行为并已限制（11140）。请在 WorkBuddy 内提交反馈申诉；本次请求不再跨账号重试，以免连坐封禁。")
					return
				}
			}
			continue
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			sawUpstreamErr = stats.UpstreamErrorFrame
			// tool_call 元数据是否每帧重复：由 config features.tool_call_meta_repeat 决定，
			// 默认 false（规范：只在首帧带 id/name）—— 重复会让 Codex 把工具调用拆成碎片。
			repeatMeta := h.cfg.Upstream != nil && h.cfg.Upstream.ToolCallMetaRepeat
			// 旁路采集"上游以 error 帧报错"的原文（上游形态：HTTP 200 已开流 + 一帧 error，
			// 如 6004 限流 / 内容拦截 / 审核）。帧仍照常透传给客户端，处置在流结束后进行。
			var errFrame string
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，这里在
			// 透传前还原成 delta.tool_calls（见 upstream/dsml.go）。仅当客户端声明过
			// tools 时才挂修复器——未声明时整条流零改写，与修复前逐字节一致。
			streamOpts := []upstream.StreamOption{
				upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
			}
			var repair *upstream.MarkupRepair
			if len(declaredTools) > 0 {
				// 名单非空 → 严格判定；名单已含会话历史里出现过的工具名，实测泄漏场景
				// 里它几乎总在（tools 声明丢失时历史仍在），故不需要弱判定兜底。
				repair = upstream.NewMarkupRepair(declaredTools, false)
				streamOpts = append(streamOpts, upstream.WithMarkupRepair(repair))
			}
			serr := upstream.StreamContextOpt(r.Context(), w, stats, repeatMeta, streamOpts...)
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 打一行便于运维确认「这次不再空转」以及统计发生率。
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
				// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			if toks, ok := stats.Tokens(); ok {
				st.toks = toks
			}
			if r, ok := stats.ReasoningTokens(); ok {
				st.reasonToks = r
			}
			if errFrame != "" {
				// 上游以 error 帧报错：按**帧内容**分类并处置账号（上游 01ecb4a）。
				// 不记成功、不清 11102 负缓存（见 recordAttempt 里 sawUpstreamErr 的门），
				// 粘性也不会绑到这个号 —— 此前这些动作在读第一帧之前就做，于是一个正在
				// 限流的号被当成健康号、粘性还会把会话钉死在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel)
				st.status = http.StatusServiceUnavailable
				log.Printf("WARN: [server] stream uid=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.UID8(acct.UID), bareModel, kind, logfmt.Truncate(errFrame, 200))
			}
			// 立即关闭上游连接（通知上游 GPU 集群中止推理，秒级止血省积分）
			rc.Close()
			// 流式结果归类：HTTP 头早在首帧就已发出（改不了状态码），这里只决定**日志与用量
			// 记账**的口径（不是面板成功率的唯一来源——失败尝试数由 usage.Add 的 ok 参数决定，
			// 本次已同步为"真实结果"）。客户端取消/断开 = 499（不惩罚账号、不计失败）；
			// 上游中途读错误/空流 = 502；正常收尾 = 200。旧实现只识别取消、其余一律留 200，
			// 于是"中途截断"在聊天表格日志里伪装成成功。
			// ctx.Err() 只读一次：重复读可能前后不一致（先 nil 后 Canceled），会让
			// st.status=499 而 clientCanceled=false —— 一次客户端取消被记成失败（审查指出）。
			ctxErr := r.Context().Err()
			st.status = streamOutcomeStatus(serr, ctxErr)
			clientCanceled := st.status == 499
			switch {
			case st.status == 499:
				log.Printf("[server] chat_stream uid=%s: client stopped generation, upstream aborted in seconds", logfmt.UID8(acct.UID))
			case serr != nil:
				log.Printf("ERR: [server] chat_stream uid=%s: upstream stream error（已向客户端补 error 帧）: %v", logfmt.UID8(acct.UID), serr)
			}
			// 成功记账与会话绑定**必须晚于结果归类**（与非流式同口径）：空流 / 中途读错误
			// 会回 502，此时把账号记成功（清 fails/softStreak/sessionDeadFails、抬权重）会
			// 让它"越失败越健康"——正是本次要根除的"先记账后确认"。
			switch {
			case st.status == http.StatusOK:
				// 上游以 error 帧报错（6004 限流 / 审核 / 限流变体）时，HTTP 头早已是 200，
				// 上面的 applyErrorPolicy 已按帧内容罚过账号；此处**绝不能**再记成功——
				// NoteSuccess 会清软冷却，等于把刚写下的惩罚抹掉（2026-10-20 实测回归：
				// TestChatStreamErrorFrameDisposesAccount 失败，冷却被"成功"清空）。
				if errFrame != "" {
					log.Printf("[server] chat_stream uid=%s: upstream error frame present → skip success accounting (penalty kept)",
						logfmt.UID8(acct.UID))
					break
				}
				h.noteUpstreamSuccess(acct.UID, stickyKey)
			case clientCanceled:
				// 499：不奖也不罚，但**必须清连续软限流计数**——否则 softStreak 只增不减，
				// 下一次真实 6004 的指数退避会被陈旧计数放大（审查 A/B 实证的回归）。
				// 注意这里刻意不用 noteUpstreamSuccess：它会 successCount++，等于把
				// "响应慢到被掐断"记成账号业绩（见 NoteClientAbort 注释）。
				h.cfg.Pool.NoteClientAbort(acct.UID)
			}
			// 成本账本与资源消耗感知：末帧 usage 带 credit 与 token 总数时记录实测单价与消耗。
			credit := 0.0
			hasCredit := false
			if c, ok := stats.Credit(); ok {
				credit = c
				hasCredit = true
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
			} else if _, hasUsage := stats.Tokens(); hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit uid=%s model=%s (no cost observation)", logfmt.UID8(acct.UID), bareModel)
			}
			st.credit = credit
			st.hasCredit = hasCredit
			delta := stats.Usage()
			delta.Credit = credit
			delta.HasCredit = hasCredit
			recordAttempt(acct.UID, delta, attemptStarted, !clientCanceled && st.status == http.StatusOK, clientCanceled, stats.TTFB())
			if hasCredit || stats.TotalTokens() > 0 {
				h.cfg.Pool.NoteUsageForModel(acct.UID, credit, stats.TotalTokens(), bareModel)
				if credit > 0 {
					rem, daily, used := h.cfg.Pool.AccountCreditsDaily(acct.UID)
					log.Printf("[credit] 账号 %s 消耗 %.2f credit (tok=%d, model=%s) | 剩余=%d, 今日消耗=%.2f, 累计消耗=%.2f",
						logfmt.UID8(acct.UID), credit, stats.TotalTokens(), bareModel, rem, daily, used)
				}
			} else {
				h.cfg.Pool.NoteUsageForModel(acct.UID, 0, 0, bareModel)
			}
			return
		}
		resp, err := upstream.AggregateContext(r.Context(), rc)
		rc.Close()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, true, 0)
				st.status = 499
				// 与流式 499 同口径：清连续软限流计数，但**不**累加成功数（取消不是账号业绩）。
				// 不这么做的话，非流式客户端（很多 SDK 默认 stream=false）超时取消越频繁，
				// softStreak 越只会累积 → 下一次真实 6004 的指数退避被陈旧计数放大（审查 F1）。
				h.cfg.Pool.NoteClientAbort(acct.UID)
				log.Printf("[server] chat uid=%s: client cancelled before completion", logfmt.UID8(acct.UID))
				return
			}
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, attemptStarted, false, false, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			// 与轮转失败路径同口径地 fail()（释放租约 + 解绑该会话的粘性号）：这次请求
			// 没有任何产出，没有理由让会话继续钉在这个号上——解绑后下一轮重新选号。
			fail(acct.UID)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「该 choice 没有结构化 tool_calls」且「finish_reason 为 stop」时生效
		// （见 upstream/dsml.go）。仅当客户端声明过 tools 时才挂修复器——未声明时
		// 修复器整体禁用，聚合响应一个字节都不动。
		if len(declaredTools) > 0 {
			mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, false)
			if n := mrepair.Converted(); n > 0 {
				log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					bareModel, n, len(declaredTools))
			} else if m := mrepair.Seen(); m > 0 {
				log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					bareModel, m, len(declaredTools))
			}
		}
		// 到这里才算"本轮真的成功"：聚合已完成，客户端将拿到完整响应。成功记账与会话
		// 绑定**必须晚于聚合**——否则聚合失败（回 502、客户端拿不到任何输出）时账号仍被
		// 记成功、会话还被钉在这个没产出内容的号上（污染成功率权重与粘性路由）。
		h.noteUpstreamSuccess(acct.UID, stickyKey)
		delta := usageDeltaFromResponse(resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if r := completionReasoningTokens(resp); r >= 0 {
			st.reasonToks = r
		}
		// 成本账本与资源消耗感知（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if credit, total, ok := usageCreditTotal(resp); ok {
			st.credit = credit
			st.hasCredit = true
			delta.Credit = credit
			delta.HasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
			h.cfg.Pool.NoteUsageForModel(acct.UID, credit, total, bareModel)
			if credit > 0 {
				rem, daily, used := h.cfg.Pool.AccountCreditsDaily(acct.UID)
				log.Printf("[credit] 账号 %s 消耗 %.2f credit (tok=%d, model=%s) | 剩余=%d, 今日消耗=%.2f, 累计消耗=%.2f",
					logfmt.UID8(acct.UID), credit, total, bareModel, rem, daily, used)
			}
		} else if st.toks > 0 {
			h.cfg.Pool.NoteUsageForModel(acct.UID, 0, st.toks, bareModel)
		} else {
			h.cfg.Pool.NoteUsageForModel(acct.UID, 0, 0, bareModel)
		}
		recordAttempt(acct.UID, delta, attemptStarted, true, false, 0)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// 轮转耗尽：按**全部**失败的性质决定终态，而不是一律 503，也不是"最后一次"。
	//
	// 为什么必须分流：畸形请求体（ErrBadParams：11101/11133 / 上下文超限 / too many tokens）
	// 换多少账号都是同样的拒绝，归 **400** 才能让 OpenAI SDK 正确判定"不要重试"；一律 503
	// 会让 SDK 反复重试，既放大消耗，又用 "all accounts unavailable (cooling/disabled)" 把
	// 责任推给实测健康的账号池（§R.8 遗留项）。
	//
	// 但"模型在该后端不可用"（ErrModelBlocked/11102）**不能**无条件判 400：MaxRotate(3)
	// 可能小于池内健康号数（实测 cn 4 个），未试过的号可能正好有该模型 —— 项目自己的
	// 枚举注释与 hint 都是 "switch model or retry on another account"。只有"该 realm 的
	// 候选都试过了"才是定论（见 modelUnavailableFinal）。
	// 其余形态仍是 503（=客户端稍后重试是对的）：429 软限流 / 熔断 / 账号故障 / 传输错误 /
	// 池里真的没号，以及**未识别 4xx（ErrClient）**——后者是保守选择：它可能是账号/模型
	// 权限差异（换号能成功），也可能是客户端参数错误；归 503 不会让 SDK 永久放弃。
	// 模型级阻塞的**负缓存**分流（上游 44d6e04 的洞 / issue #102 附带发现）：
	// 一旦模型级冷却写进负缓存，后续请求在选号阶段就被挡下 —— 此时 lastErr 为空，
	// 下方的 switch 只会走默认分支报 no_healthy_account（"池子没号"），把
	// 「号都在、只是这个模型被限」误报成「账号池挂了」，上游原文与解封时间全丢。
	//
	// 分流口径（比上游更细，保留本项目既有契约）：
	//   - 有权威重置时间（6004 带「将在 … 重置」）：模型**暂时**不可用 → 503 可重试，
	//     并把解封时间带给客户端（换号无用，但等一会儿有用）；
	//   - 11102（该后端无此模型）的负缓存条目：换模型才有用 → 400 model_unavailable
	//     不可重试。判据用 **AllNotFound**（本 realm 内所有被挡条目都是 11102）—— 不能用
	//     Until 是否为零（11102 退避同样非零），也不能只看单条 Reason 前缀（混合态会随
	//     map 迭代序在 400/503 之间随机翻转，审查 D 的 P1-3）。
	//
	// 覆盖条件含 ErrSoftRate：首个请求（这轮真的打过账号、lastErr 是 6004 软限流）也应拿到
	// 模型级信息（号都在、几个被挡、何时解封），否则只有负缓存命中的后续请求才有，观感像
	// "偶发生效"（审查 A 的 P1-4）。
	// 覆盖条件还要含 ErrSoftRate / ErrModelBlocked：
	//   - ErrSoftRate：本轮打到 6004（首请求）也要拿到模型级信息；
	//   - ErrModelBlocked：负缓存写入后循环会因"无 healthy 候选"提前终止，此时 lastErr
	//     是 11102，若被守卫挡掉就会落到 sawModelBlocked 的"仍可换其他账号"503 文案 ——
	//     而实际上号都在、只是都没这个模型（实测会被 TestChatAllModelBlocked400... 抓到）。
	var softRateHit, modelBlockedHit bool
	if ue, ok := lastErr.(*upstream.Error); ok {
		switch ue.Kind {
		case upstream.ErrSoftRate:
			softRateHit = true
		case upstream.ErrModelBlocked:
			modelBlockedHit = true
		}
	}
	if modelBlock.Blocked && (lastErr == nil || softRateHit || modelBlockedHit) {
		local := fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if rs := strings.TrimSpace(modelBlock.Reason); rs != "" {
			local += "; gateway note: " + rs
		}
		if modelBlock.AllNotFound {
			// 确定性"本后端没有这个模型"：等没用，换模型才有用。
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable",
				"model is unavailable on every account (per-model negative cache, 11102), try another model; 可用模型清单见 /v1/models ("+local+")")
			st.status = http.StatusBadRequest
			return
		}
		local += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"model is temporarily unavailable on every account (per-model rate limit), retry later ("+local+")")
		st.status = http.StatusServiceUnavailable
		return
	}
	switch {
	case sawBadParams:
		msg := "request body was rejected by upstream as invalid"
		if lastErr != nil {
			// 上游原文（code/msg/requestId）：与 content_blocked/prompt_too_long 的 400 透传同口径。
			// 说明性后缀：11101 历史上曾是**网关侧**工具消息序列化缺陷（README §7），
			// 若上游判请求畸形但请求看起来是正常的，需要运维查网关改写而不是让用户瞎改。
			msg = lastErr.Error() + " (if this request looks well-formed, check gateway logs: a gateway-side rewrite regression can also produce this)"
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
		st.status = http.StatusBadRequest
	case sawModelBlocked:
		if h.modelUnavailableFinal(bareModel, realm, tried) {
			msg := "model unavailable on the reached backend(s): 若这是国际版模型，请用 global:<模型名> 前缀；" +
				"若是 11158（该用户类型无权使用此模型），说明当前账号档位不支持该模型，请换模型；可用清单见 /v1/models"
			if lastErr != nil {
				msg += "; upstream: " + lastErr.Error()
			}
			writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", msg)
			st.status = http.StatusBadRequest
			return
		}
		if h.modelMissingFromCatalog(bareModel, realm) {
			// 只**诊断**、不改判定：清单来自"双 UA 并集 + 企业端点"（2026-10-03 批次 6 起），
			// 但仍可能不全（上游按其它 UA 下发独有模型；实测 2026-10-03 双 UA 并集把 cn 目录
			// 从 19 补到 33）→ 拿"不在清单"当"模型不存在"的判据会把能用的模型硬判成 400
			// （不可重试）。这里留痕即可，便于发现拼写错误。
			log.Printf("WARN: [server] model_blocked model=%s 不在网关清单内（清单可能不完整：双 UA + 企业端点并集采集也未必覆盖上游全部，勿据此判定模型不存在）",
				logfmt.Truncate(bareModel, 64))
		}
		// 模型不可用但**尚未成定论**（仍有没试过的可用账号，或池此刻整体不可用）：
		// 保持可重试语义，但文案必须说清是"模型在这几个号上不可用"而不是"账号池挂了"。
		msg := "model was rejected by the reached account(s) as unavailable; the gateway can still try other accounts — retry, or switch model (use global:<id> for international models)"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
		st.status = http.StatusServiceUnavailable
	case errors.Is(lastErr, errUpstreamTimeout):
		// 超时已在传输层分支止损（见 isUpstreamTimeout）。这里给一条**可区分**的文案：
		// 混进"没有可用账号"会把排查方向带偏（上游慢 vs 号池坏）。
		msg := "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_timeout", msg)
		st.status = http.StatusServiceUnavailable
	default:
		msg := "all accounts unavailable (cooling/disabled)"
		if lastErr != nil {
			msg += ": " + lastErr.Error()
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
		st.status = http.StatusServiceUnavailable
	}
}

// modelUnavailableFinal 判断"模型不可用"是否已是**定论**（可以回 400 让客户端别再换号重试）：
// 该 realm 此刻**有**可用候选，且它们**全部**已被本轮试过。任何未试过的候选都可能正好有
// 这个模型，此时保持 503（可重试）才是正确语义。
//
// 两个刻意的保守点：
//   - 候选为空（全冷却 / 在途占满 / 全禁用）时返回 **false**：那是"池此刻不可用"，
//     不是"模型不存在"——账号恢复后同一个模型可能就能用了，回 400 会让客户端永久放弃。
//   - 只认"此刻可用"的候选集（`AvailableUIDsForModelRealm` 已剔除满额与模型级限流的号）。
//
// 已知边界：池内可用号数 > MaxRotate(3) 时，单次请求永远试不完全部候选 → 该模型始终
// 回 503（可重试，文案已说明是模型问题）。真正的根治是"模型级负缓存"（见
// upstream.ErrModelBlocked 的枚举注释），需单独立项。
func (h *Handler) modelUnavailableFinal(bareModel, realm string, tried map[string]bool) bool {
	cands := h.cfg.Pool.AvailableUIDsForModelRealm(bareModel, realm)
	if len(cands) == 0 {
		return false
	}
	for _, uid := range cands {
		if !tried[uid] {
			return false
		}
	}
	return true
}

// dumpReqDump 把请求体落到调试文件。路径解析见调用点注释；失败只记日志（节流）。
var dumpReqFails atomic.Int64

func dumpReqDump(v string, body []byte) {
	path := filepath.Join("data", "last_request.json")
	if strings.ContainsAny(v, `/\\`) {
		path = v // 显式给了路径：以其为准（Docker 可继续用 /app/data/...）
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// 目录建不出来就没什么可做的：节流打日志（每 20 次一条），其余静默。
			if n := dumpReqFails.Add(1); n == 1 || n%20 == 0 {
				log.Printf("ERR: [server] dump req: mkdir %s: %v (第 %d 次)", dir, err, n)
			}
			return
		}
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		if n := dumpReqFails.Add(1); n == 1 || n%20 == 0 {
			log.Printf("ERR: [server] dump req: write %s: %v (第 %d 次)", path, err, n)
		}
	}
}

// needsUpstream 报告该路由是否依赖上游客户端（用于 nil-Upstream 的依赖兜底：
// /healthz 是存活探针，故意排除在外）。
func needsUpstream(path string) bool {
	switch path {
	// 只列**真正解引用 h.cfg.Upstream** 的路由（/status 只读账号池，不依赖上游客户端）。
	case "/v1/models", "/v1/chat/completions":
		return true
	}
	return false
}

// modelMissingFromCatalog 报告"该模型名不在网关对外公布的清单里"（**仅用于诊断日志**，
// 绝不作为"模型不存在"的判定依据）。
//
// 为什么不能拿它下判定（2026-10-03 实测，见路线图 §T.9）：
// 网关的 cn 清单来自**单次 UA** 的 /v3/config 拉取，而同一端点对不同 UA 下发不同模型集合
// （global 侧正因此才用双 UA 取并集）。实测生产清单只含 10 个 cn 裸名，与权威清单
// （19 个在用模型）的交集仅 12 个 —— `deepseek-v4.1-flash`、`glm-5.3`、`hy4-preview`、
// `kimi-k2.6` 等**正在使用**的模型都不在网关清单里。若按"不在清单即 400"，这些模型会被
// 硬判为不可用（且不可重试），直接把可用配置打断。根治办法是把 cn 清单也改成双 UA 取并集
// （见 §T.9 待办），在那之前"未知模型"只能维持 503 + 可操作的文案。
//
// 两个保守门槛（都不满足就返回 false）：
//   - **仅 cn realm**：global 的清单来自未缓存的上游探测（fetchGlobalModels 每次实拉），
//     无法在错误路径上低成本取到权威全集，故不下结论；
//   - **动态清单必须新鲜**：只有 10 分钟 TTL 内的动态拉取结果才说明"这份清单就是当前
//     上游目录"。缓存空/过期（上游目录接口故障）时静态表可能不是全集，宁可回 503。
//
// 注意 bare 裸名解析到 cn：清单里 global 模型也以裸名列出（DSH 等客户端按裸名调用），
// 但裸名出站走 cn、cn 后端不提供国际模型 → 这里判"不在 cn 清单"并回 400 是**正确**答案
// （文案里给出 global:<id> 的修法），比让客户端无限重试 503 更有用。
func (h *Handler) modelMissingFromCatalog(bareModel, realm string) bool {
	if bareModel == "" || (realm != "" && realm != "cn") {
		return false
	}
	dynamicModelsCache.RLock()
	fresh := len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL
	ids := dynamicModelsCache.ids
	dynamicModelsCache.RUnlock()
	if !fresh {
		return false
	}
	for _, mi := range ids {
		if mi.ID == bareModel {
			return false
		}
	}
	for _, m := range staticModels {
		if id, ok := m["id"].(string); ok && id == bareModel {
			return false
		}
	}
	return true
}

// sameOriginRequest 判断请求来源是否可信：
//   - 无 Origin 头（curl / SDK / 同源导航）→ 放行；
//   - Origin 的 host:port 与请求 Host 相同（localhost / 127.0.0.1 / ::1 互相视为本机）→ 放行。
//
// 仅用于"api_key 为空"场景的第二道条件，判据主体见 keylessRequestAllowed（Host 必须是回环）。
func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	oh, op := hostPortWithDefaults(u.Host, u.Scheme == "https")
	rh, rp := hostPortWithDefaults(r.Host, r.TLS != nil)
	if op != rp {
		return false
	}
	if oh == rh {
		return true
	}
	// 本机别名互换：面板可能用 localhost 打开、而请求 Host 是 127.0.0.1（反之亦然）。
	return isLoopbackHost(oh) && isLoopbackHost(rh)
}

// keylessRequestAllowed 在 **api_key 为空（网关不鉴权）** 时判断请求能否放行。
//
// 判据分两层：
//  1. **无 Origin** → 放行。Origin 是浏览器在跨源请求上才发的头（fetch/XHR/表单 POST 都发），
//     curl / OpenAI SDK / 探活器 / 反代都不会发；非浏览器客户端没有 CSRF 面，也就没有
//     "被其它网页借道烧积分"的风险。这条同时保证"本机工具的既有用法"零回归。
//  2. **有 Origin**（= 浏览器发起的跨源/同源请求）→ 要求 Host 指向本机回环 **且** 与 Origin 同源。
//
// 为什么第 2 层必须查 Host，而不能只比 Origin==Host：DNS rebinding 攻击下两者**同为攻击者
// 控制的域名**（攻击者页开在 http://attacker.tld:17963，该域 DNS 先解析到攻击者、再重绑定到
// 127.0.0.1），实测 `Host: evil.com:17963` + `Origin: http://evil.com:17963` 会被判同源放行。
// Host 头由浏览器按地址栏填写、页面 JS 无法伪造，所以"Host ∈ 回环"才是可靠的本机判据。
func keylessRequestAllowed(r *http.Request) bool {
	// CSRF 兜底（2026-10-03 加固）：`Origin` 同源检查依赖浏览器发送 Origin —— 老/嵌入式
	// WebView 可能不发，于是跨站表单提交/图片请求可能驱动本机网关改状态（关机/删号/改配置/
	// 甚至把攻击者账号注册进池）。表单与 `<img>` **都无法设置自定义请求头**，所以这里要求
	// keyless 模式下的**面板改状态请求**必须带 `Authorization` 头（值不校验——它就是
	// "非表单客户端"的证明；配了 api_key 时本函数根本不会被调用，鉴权走正常 Bearer 校验）。
	//
	// 早期实现曾按 Content-Type 拒掉"表单类型"，但那会**误伤面板自己的文件导入**
	// （`/panel/api/auths/import` 就是 multipart/form-data）——已改为头校验。
	if isPanelStateChanging(r.URL.Path, r.Method) && r.Header.Get("Authorization") == "" {
		return false
	}
	// /v1 侧另有一条正交的约束：**不接受表单编码**。面板从不对 /v1 用表单类型（合法客户端
	// 一律发 JSON），而 keyless + 老浏览器（不发 Origin）下，一个 `text/plain` 表单就能构造出
	// 形似 JSON 的 body 去打 /v1/chat/completions —— 攻击者读不到响应，但**额度照烧**。
	// 这里只拒表单类型，不要求自定义头（后者会破坏"不带 Authorization 的 keyless SDK"）。
	if isV1APIPath(r.URL.Path) && r.Method != http.MethodGet && r.Method != http.MethodHead &&
		isFormContentType(r.Header.Get("Content-Type")) {
		return false
	}
	if r.Header.Get("Origin") == "" {
		return true
	}
	if !isLoopbackHost(r.Host) {
		return false
	}
	return sameOriginRequest(r)
}

// isPanelAPIPath 报告路径是否是面板数据接口（/panel/api/...）。面板 HTML/静态资源与其
// 它 /v1 端点不受"keyless 写操作需带头"这条约束影响。
func isPanelAPIPath(path string) bool {
	return strings.HasPrefix(path, "/panel/api/")
}

// isV1APIPath 报告路径是否是 /v1 数据接口。
func isV1APIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

// isPanelStateChanging 报告该面板请求是否会**改状态**（含唯一一处"改状态的 GET"）。
//
// `GET /panel/api/login/poll` 是特例：它写 auths 文件、把账号加入池，并可能触发签到/试用
// 链路（panel/login.go），因此必须与写操作同口径 —— 否则一个 `<img src=...>` 就能驱动它
// （唯一屏障只剩不可猜的 state；state 一旦从 referrer/日志泄漏即被利用）。
func isPanelStateChanging(path, method string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return strings.HasPrefix(path, "/panel/api/login/poll")
	}
	return isPanelAPIPath(path)
}

// isFormContentType 报告 Content-Type 是否是**表单**类型（HTML form 三种 enctype 之一）。
// 参数（charset/boundary）先剥离再比较。
func isFormContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch ct {
	case "application/x-www-form-urlencoded", "multipart/form-data", "text/plain":
		return true
	}
	return false
}

// hostPortWithDefaults 拆出 host/port；缺端口时按 scheme 补默认端口（80/443），
// host 做归一（小写 / 去 IPv6 方括号 / 去尾点）。
func hostPortWithDefaults(hostport string, isTLS bool) (string, string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return hostOnly(h), p
	}
	if isTLS {
		return hostOnly(hostport), "443"
	}
	return hostOnly(hostport), "80"
}

// hostOnly 归一主机名：小写、去 IPv6 方括号、去尾点（`localhost.` 与 `localhost` 等价，
// 浏览器把尾点 FQDN 原样放进 Host/Origin，不归一会让面板写操作被误判为跨站）。
func hostOnly(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]")), ".")
}

// isLoopbackHost 判断 host:port 是否指向本机回环：localhost、127.0.0.0/8、::1。
// **不含 0.0.0.0**：它是"绑定任意本地接口"的通配语义，不是回环别名。
func isLoopbackHost(hostport string) bool {
	host, _ := hostPortWithDefaults(hostport, false)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// streamOutcomeStatus 把一次流式透传的结束状态归类为**记录用**状态码（不改变已发出的 HTTP 响应）：
//   - serr == nil 且请求上下文无错 → 200（正常收尾）；
//   - 客户端取消/断开（serr 是 context.Canceled，或请求上下文已出错）→ 499（不惩罚账号）；
//   - 其余非空 serr（上游中途读错误、空流）→ 502。
//
// 单独抽成纯函数是为了能直接单测归类口径（原实现只有"取消→499，其余留 200"）。
func streamOutcomeStatus(serr, ctxErr error) int {
	switch {
	case errors.Is(serr, context.Canceled) || ctxErr != nil:
		return 499
	case serr != nil:
		return http.StatusBadGateway
	default:
		return http.StatusOK
	}
}

// noteUpstreamSuccess 记录"本轮上游调用成功"的两个副作用：
//   - 账号成功计数（NoteSuccess 会清 fails / retryCount / softStreak / sessionDeadFails）；
//   - 粘性跟随最终成功号：把会话重绑到实际成功的账号，多轮对话下一跳不再随机抽。
//
// 时点很关键：流式在首帧透传时成立；非流式必须等聚合成功（见 chatCompletions 调用点）。
func (h *Handler) noteUpstreamSuccess(uid, stickyKey string) {
	h.cfg.Pool.NoteSuccess(uid)
	if stickyKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(stickyKey, uid)
	}
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：内容拦截会立即 400 返回，其余种类 continue 换号。
//
// 八条路径，各司其职（含 ErrModelBlocked 的 11102 负缓存）：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError）；passthrough 首遇触发
//     降级重试，最终仍拦则回 400 content_blocked（防火墙文案，不含账号/错误码）。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrModelBlocked:
		// 只跳过**路由/容量类**文案（"no available channel" 这类可能自愈的；审查 D 的 P2-9），
		// 其余一律落负缓存 —— 222 注意**不能**反过来只认 body 里含 "11102"：ErrModelBlocked
		// 还有 404+文案、4xx+文案族两条生产者，排除它们会让账号不被 modelCooled ⇒ 每次请求
		// 继续烧号，且 >MaxRotate 的池对这些模型永久 503（审查 E 的 P1-2）。
		if upstream.IsTransientModelBlock(body) {
			return
		}
		// 11102「该后端无此模型」：写 (账号, 模型) 负缓存（6h 起指数退避、封顶 24h）。
		// 作用：后续同模型请求不再反复烧该号；ModelBlocked 因此能给出"换模型才有用"的 400
		// （上游 44d6e04）。模型实测又通则由成功路径的 BlockModelClear 清掉。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrWafBlock:
		// 上游 WAF 拦截页（403 + HTML）：出口被风控，不是账号故障也不是请求错误。
		// 语义是"稍后重试" ⇒ 软冷却 + 抖动退避（90~120s），让轮转/重试避开这段窗口。
		jitter := time.Duration(time.Now().UnixNano()%30) * time.Second
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, 90*time.Second+jitter, "upstream WAF 403")
	case upstream.ErrSoftRate:
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		modelRateLimited := upstream.IsModelRateLimit(body)
		if modelRateLimited {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "429 rate limit")
		if modelRateLimited {
			// 6004 但无重置时间：只挂审计台账供账号页展示，不改变选号行为。
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrChannelRefused:
		// 11128 通道校验失败（上游认为请求不是官方客户端发出）：账号本身健康，
		// 不冷却、不熔断、不计错，交给轮转继续；全局性通道问题由上层最终返回。
		log.Printf("[pool] channel refused (11128) uid=%s：请求未通过通道校验，账号不罚", logfmt.UID8(uid))
		return
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。/status 以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		// 11128（通道校验失败）与 11140（账号被限制）msg 同为 "request illegal"，
		// Classify 已按业务码分流 ⇒ 这里只处理真正的 11140（code + 文案双条件）。
		if upstream.IsAccountRestricted(body) {
			// 【P1-7】用 Restrict（带到期时间）而不是 Disable：上游这类限制会自行解除，
			// 永久禁用会让恢复后的健康号白白浪费，并诱导用户去点"解冻"。
			h.cfg.Pool.Restrict(uid, h.cfg.RestrictionTTL,
				"account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 首遇由 chatCompletions 内降级重试处理；最终仍拦则回 400
		// content_blocked（防火墙文案），不再轮转、不暴露账号/冷却/错误码。
	case upstream.ErrPromptTooLong, upstream.ErrImageInvalid:
		// 11115「prompt is too long」与 11135「invalid image」：
		// 请求的问题不是账号的问题（同一 body 换任何号都超限或解析失败）。
		// 零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的问题，
// 换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
//
// 三态判定：
//  1. net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）；
//  2. 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）；
//  3. **客户端仍在但 ctx 被取消** —— 那只能是我们自己的空闲看门狗掐的流，即上游停滞
//     （客户端主动断连时 r.Context() 已取消 ⇒ clientGone=true ⇒ 走抖动分支）。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	typ := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusNotFound:
		typ = "invalid_request_error"
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	}
	// 【D2，2026-10-21】503 统一带 Retry-After。
	//
	// 503 在本网关的语义是"暂时没有可用账号（冷却/熔断/在途占满/模型暂不可用）"，
	// 即**可以重试**；但此前不带任何退避提示，客户端（OpenAI SDK / zcode / 各类 Agent）
	// 只能盲目重试——实测 10 并发时 8 个 503，若无退避会立刻打出第二轮、第三轮，
	// 把"暂时不可用"放大成持续上游压力（也正是风控最敏感的那种密集调用）。
	// 5 秒是本项目软冷却基数（600s）与单轮上游耗时之间的折中：既能明显降低重试频次，
	// 又不会让客户端等太久。
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"code":    code,
		},
	})
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// resolveCacheConvID —— 出站 prompt_cache_key 的会话哈希源（body → X-Conversation-ID → 会话 key）。
// 移植上游 20254d8：不带 conversation id 的客户端（Anthropic / Claude Code 类）若留空，上游
// prompt_cache_key 会退化成"每账号一个常量"⇒ 同账号多客户端共用一个前缀缓存槽并互相驱逐 ⇒ 命中率下降。
func resolveCacheConvID(body []byte, hdr http.Header, sessKey string) string {
	if v := session.ResolveConversationID(body); v != "" {
		return v
	}
	if v := strings.TrimSpace(hdr.Get("X-Conversation-ID")); v != "" {
		return v
	}
	return sessKey
}

// 【P1-7】上游风控限制的记账与整池波次暂停。

// noteRestriction 记录一次 11140 账号限制；同 realm 在窗口内达到阈值时暂停该域一段时间。
func (h *Handler) noteRestriction(a *auth.Auth) {
	if a == nil {
		return
	}
	realm := a.Realm()
	now := time.Now()
	h.restrictMu.Lock()
	defer h.restrictMu.Unlock()
	if h.restrictHits == nil {
		h.restrictHits = make(map[string][]time.Time)
	}
	kept := h.restrictHits[realm][:0]
	for _, t := range h.restrictHits[realm] {
		if now.Sub(t) <= h.cfg.RestrictionWaveWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	h.restrictHits[realm] = kept
	if len(kept) < h.cfg.RestrictionWaveMin {
		return
	}
	if h.realmPausedUntil == nil {
		h.realmPausedUntil = make(map[string]time.Time)
	}
	h.realmPausedUntil[realm] = now.Add(h.cfg.RestrictionPause)
	h.restrictHits[realm] = nil
	log.Printf("WARN: [server] realm=%s 在 %s 内已有 %d 个账号被上游限制 ⇒ 整池暂停 %s（避免继续试探连坐）",
		realm, h.cfg.RestrictionWaveWindow, len(kept), h.cfg.RestrictionPause)
}

// realmPaused 报告该 realm 是否处于风控波次暂停中。
func (h *Handler) realmPaused(realm string) bool {
	h.restrictMu.Lock()
	defer h.restrictMu.Unlock()
	until, ok := h.realmPausedUntil[realm]
	return ok && time.Now().Before(until)
}
