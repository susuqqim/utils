// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// Config 顶层配置。
// ReqLog 请求指标采集配置。口径（用户批准）：内存环 500 条 + 磁盘归档 NDJSON，
// 保留 retain_days 天且 ≤max_mb；IP 截断 /24（IPv6 /48）、UA 截断 120、uid 8 位；
// **不落** prompt/响应正文。**默认关闭**（opt-in，零回归）。
type ReqLog struct {
	Enabled    bool   `json:"enabled"`
	Dir        string `json:"dir,omitempty"`
	RetainDays int    `json:"retain_days,omitempty"` // 0 = 默认 7 天
	MaxMB      int    `json:"max_mb,omitempty"`      // 0 = 默认 50MB
}

// StickyCfg 是 session_sticky 的配置。
//
// 2026-10 兼容性修复：早期版本把 session_sticky 写成**布尔**（"session_sticky": true），
// 升级到新版本后 Go 的严格解码会报 "cannot unmarshal bool into ... struct"，导致
// 网关**启动即失败**（老 config 不会被安装程序覆盖，见 MSI 的 CaSeedConfig）。
// 这里自定义 UnmarshalJSON：布尔 true/false ⇒ 等价于 {enabled: true/false} 且用默认 TTL/GC。
type StickyCfg struct {
	Enabled    bool   `json:"enabled"`     // 默认 true
	TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
	GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
}

// UnmarshalJSON 同时接受对象（新格式）与布尔（旧格式）。
//
// 关键：必须以**现有值**为底解码（`tmp := plain(*s)`）。ParseConfig 会先注入默认值再
// 反序列化，若这里从零值开始，像 `{"session_sticky":{"ttl":"0s"}}` 这种只写一个字段的
// 配置会丢掉 gc_interval 的默认值 ⇒ 后续校验报 "invalid duration"""（曾把
// TestNegativeDurationsRejected 干挂）。布尔旧格式同理：只覆盖 enabled，保留默认 TTL/GC。
func (s *StickyCfg) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "true") || strings.HasPrefix(trimmed, "false") {
		cur := *s
		cur.Enabled = strings.HasPrefix(trimmed, "true")
		if cur.TTL == "" {
			cur.TTL = "30m"
		}
		if cur.GCInterval == "" {
			cur.GCInterval = "5m"
		}
		*s = cur
		return nil
	}
	type plain StickyCfg
	tmp := plain(*s) // 以现有（默认）值为底
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	*s = StickyCfg(tmp)
	return nil
}

// PlaceholderAPIKey 是 config.example.json 里的占位符。新装用户拿到的 config 里就是它，
// 若把它当真密钥，用户既不知道密钥、又无法登录控制台（会被 401 拦住），最终卡死。
// 因此加载时按"未设置"处理（等同于空 = 不鉴权），并让控制台自动弹出首次初始化提示。
const PlaceholderAPIKey = "CHANGE_ME_BEFORE_USE"

type Config struct {
	Listen string `json:"listen"`  // ":7863"
	APIKey string `json:"api_key"` // 空 = 不鉴权
	// APIKeyPrevious 过渡密钥（密钥轮换期）：非空时**也被接受**，让"已启动、把旧钥缓存在
	// 内存里"的客户端不至于立刻 401；用它的请求会打节流 WARN 留痕。
	// 客户端全部迁移后**必须清空**（否则旧钥仍有效 = 轮换等于没做）。
	APIKeyPrevious string `json:"api_key_previous,omitempty"`
	AuthDir        string `json:"auth_dir"`   // ./auths
	StateFile      string `json:"state_file"` // ./data/state.json

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB，默认 8）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		// 0/负数视为非法 → normalize 回落默认并记录。
		MaxBodyMB int `json:"max_body_mb"`
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（上游 15a6fc9，issue #100）：
		// http.Server 的 ReadTimeout 覆盖整个请求读取，大上下文/文件块请求经反代链转发时上传可能
		// 超过旧固定值而被掐（客户端拿到 400 read body: … i/o timeout）。缺省 "300s"；"0" = 不限制；
		// 负值/非法 fail fast。**改动需重启进程**。
		ReadTimeout string `json:"read_timeout"`
	} `json:"server"`
	// ReqLog 请求指标采集（默认关闭；口径见 ReqLog 类型注释）。
	ReqLog ReqLog `json:"reqlog"`
	// EnterpriseGate 企业号能力门控（S5 第 2 步，上游 7e3bc51）：开启后，企业版账号
	// （auth 带非空 enterpriseId）跳过**个人成长体系**类任务（成长队列/任务中心/每日真实对话/
	// 夜猫子）—— 企业号没有个人成长体系，网关此前会每天空发注定 400/10001 的请求。
	// **默认 false（关闭）**：只读展示不受影响；开启前需用户确认（涉及行为变更）。
	EnterpriseGate bool `json:"enterprise_gate"`
	// HiddenModels 网关侧隐藏的模型名（默认空 = 零回归）。用途：上游目录里
	// 长期保留着一些"有名无服务"的模型（直接请求得上游 11102 service info not found），客户端枚举
	// 后逐个尝试 ⇒ 每次都在池里写下 6h→12h→24h 的模型级负缓存、面板一片"预计 X 后重试"。
	// 配置后：/v1/models 不再列出；直接请求**本地 400 model_hidden**（不打上游、不计费、不写冷却）。
	HiddenModels []string `json:"hidden_models,omitempty"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 内置默认（对齐官方 5.5.4 分发包）；
		// 显式配置（如升级后的桌面包版本）则随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.137.1）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= 旧行为：X-Product="SaaS"，不设 X-IDE-*（避免行为突变）。
		// 配 "WorkBuddy" 则三头跟随该值，匹配官方桌面端用量归因。
		// Identity 出站身份预设：""（默认，历史行为）/ "wb" / "vscode" / "cli"。
		// 上游 2026-10-21 收紧渠道校验（11128）后，"用 vsc 或 CLI 出站"是社区实测的绕行手段。
		Identity string `json:"identity"`
		// PrivateData X-Private-Data（「数据私有」声明）：""/"auto" = 按出站身份的官方默认；
		// "true" = 数据私有（不参与模型优化）；"false" = 允许参与模型优化。
		// 注意语义方向：true 更隐私，与官方内部字段 enableModelOptimization 正相反。
		PrivateData string `json:"private_data"`
		ClientName  string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// ToolCallMetaRepeat 兼容开关（默认 false）：流式 tool_calls 的
		// id/type/function.name 是否在**每个续帧**都重复带上。
		//   false（规范）= 只在首帧带；续帧只带 index + arguments。
		//   true（旧行为）= 每帧重复（为"状态机需要重复元数据"的老客户端保留）。
		// 默认 false 的原因：重复元数据会让按"带 id 即新调用"解析的客户端（实测 Codex）
		// 把一次工具调用拆成几百个碎片（实测 327 帧 / 每帧 1~11 字符 → 无法正常发送代码）。
		ToolCallMetaRepeat bool `json:"tool_call_meta_repeat"`
	} `json:"features"`

	UI struct {
		// Tray 是否在 Windows 上启用系统托盘图标（默认 true）。
		// 启用时进程会隐藏控制台窗口，通过托盘菜单操作面板 / 重启 / 开机自启 / 退出；
		// 置 false 或启动加 -no-tray 可回到纯后台无托盘模式（非 Windows 平台恒不生效）。
		// 切片指针：区分"未配置"（走默认 true）与"显式 false"。
		Tray *bool `json:"tray,omitempty"`
	} `json:"ui"`

	Prompt struct {
		// Mode custom（默认）= 网关用自有系统提示词替换客户端 system/developer；
		// passthrough = 透传客户端原始 system（降级重试仍会切到 Degraded）。
		Mode string `json:"mode"` // "custom" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 【P1-7】上游风控限制（11140 账号画像风控）防连坐三件套
		RestrictedRotateMax   int     `json:"restricted_rotate_max"`   // 0（默认）= 收到 11140 即停手，不跨号重试
		RestrictionTTL        string  `json:"restriction_ttl"`         // 限制持久化时长，默认 "2h"（到期自动放回池）
		RestrictionWaveWindow string  `json:"restriction_wave_window"` // 波次统计窗口，默认 "15m"
		RestrictionWaveMin    int     `json:"restriction_wave_min"`    // 窗口内被限制多少个号触发整池暂停，默认 2
		RestrictionPause      string  `json:"restriction_pause"`       // 整池暂停时长，默认 = 窗口
		IdleWeightPerHour     float64 `json:"idle_weight_per_hour"`    // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax         float64 `json:"idle_weight_max"`         // 闲置补偿封顶，默认 5.0
		// Phase 3 加权调度与分块策略
		ChunkQuotaCredits   float64 `json:"chunk_quota_credits"`   // 单账号连续消耗分块额度，默认 25.0，<=0 禁用分块
		CriticalExpireHours float64 `json:"critical_expire_hours"` // 紧急清仓层小时数，默认 48.0
		UrgentExpireHours   float64 `json:"urgent_expire_hours"`   // 临期清仓层小时数，默认 72.0
		NearExpireDays      float64 `json:"near_expire_days"`      // 近期临期层天数，默认 7.0
		SafeReserveDays     float64 `json:"safe_reserve_days"`     // 安全底仓层天数，默认 10.0
		LowCreditCutoff     float64 `json:"low_credit_cutoff"`     // 枯竭断崖保护阈值，默认 800.0
		ExpireUrgencyWeight float64 `json:"expire_urgency_weight"` // 到期紧迫度权重加成满分，默认 15.0
		ExpirePower         float64 `json:"expire_power"`          // 到期天数非线性加速指数，默认 1.5

		// 自适应分块参数（可选）
		AdaptiveChunking       *bool   `json:"adaptive_chunking,omitempty"`         // 是否启用自适应三档分块（默认 true）
		TierUnknownQuota       float64 `json:"tier_unknown_quota,omitempty"`        // 冷启动未知单价模型分块额度，默认 15.0
		TierUnknownMaxReqs     int     `json:"tier_unknown_max_reqs,omitempty"`     // 冷启动未知单价模型最大轮数，默认 0
		TierCheapCostCutoff    float64 `json:"tier_cheap_cost_cutoff,omitempty"`    // 极省档单价阈值，默认 0.08
		TierFlagshipCostCutoff float64 `json:"tier_flagship_cost_cutoff,omitempty"` // 旗舰档单价阈值，默认 0.80
		TierCheapQuota         float64 `json:"tier_cheap_quota,omitempty"`          // 极省档分块额度，默认 25.0
		TierCheapMaxReqs       int     `json:"tier_cheap_max_reqs,omitempty"`       // 极省档最大轮数，默认 60
		TierNormalQuota        float64 `json:"tier_normal_quota,omitempty"`         // 常规档分块额度，默认 50.0
		TierNormalMaxReqs      int     `json:"tier_normal_max_reqs,omitempty"`      // 常规档最大轮数，默认 200
		TierFlagshipQuota      float64 `json:"tier_flagship_quota,omitempty"`       // 旗舰档分块额度，默认 100.0
		TierFlagshipMaxReqs    int     `json:"tier_flagship_max_reqs,omitempty"`    // 旗舰档最大轮数，默认 100

		// Direction 2 柔性透气轮转参数
		PacedBreathingEnabled    *bool `json:"paced_breathing_enabled,omitempty"`     // 是否启用柔性透气轮转（默认 true）
		PacedTier2ConsecutiveMax int   `json:"paced_tier2_consecutive_max,omitempty"` // Tier 2 连续服务分块上限（默认 3）
		// CreditFloor 积分保底：账号余额低于该值时对**实测收费**模型不再参与选号。
		// 0 = 关闭（默认，零回归）；负值钳 0。语义见 pool.Pool.creditFloor / floorBlockedForRealmModel。
		CreditFloor int64 `json:"credit_floor,omitempty"`
	} `json:"pool"`

	SessionSticky StickyCfg `json:"session_sticky"`

	// 解析后
	SoftRateDur            time.Duration `json:"-"`
	SoftRateMaxDur         time.Duration `json:"-"`
	BreakerCooldownDur     time.Duration `json:"-"`
	BreakerCooldownMaxD    time.Duration `json:"-"`
	RestrictionTTLD        time.Duration `json:"-"`
	RestrictionWaveWinD    time.Duration `json:"-"`
	RestrictionPauseD      time.Duration `json:"-"`
	SessionTTL             time.Duration `json:"-"`
	SessionGCInterval      time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // 请求体上限默认 8MB
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Features.ToolCallMetaRepeat = false // 规范优先；置 true 恢复"每帧重复元数据"的旧行为
	trayDefault := true                   // 独立取址：不与其它 *bool 默认值共享指针
	c.UI.Tray = &trayDefault
	c.Prompt.Mode = "custom" // 缺省 custom：网关自有提示词从源头消灭 system 指纹误报
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ChunkQuotaCredits = 25.0
	c.Pool.CriticalExpireHours = 48.0
	c.Pool.UrgentExpireHours = 72.0
	c.Pool.NearExpireDays = 7.0
	c.Pool.SafeReserveDays = 10.0
	c.Pool.LowCreditCutoff = 800.0
	c.Pool.ExpireUrgencyWeight = 15.0
	c.Pool.ExpirePower = 1.5
	c.Pool.TierUnknownQuota = 15.0
	c.Pool.TierUnknownMaxReqs = 0
	c.Pool.TierCheapCostCutoff = 0.08
	c.Pool.TierFlagshipCostCutoff = 0.80
	c.Pool.TierCheapQuota = 25.0
	c.Pool.TierCheapMaxReqs = 60
	c.Pool.TierNormalQuota = 50.0
	c.Pool.TierNormalMaxReqs = 0
	c.Pool.TierFlagshipQuota = 100.0
	c.Pool.TierFlagshipMaxReqs = 0
	trueVal := true
	c.Pool.PacedBreathingEnabled = &trueVal
	c.Pool.PacedTier2ConsecutiveMax = 3
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
// maxBodyMBLimit server.max_body_mb 的上限：见 normalize 中的溢出说明。
const maxBodyMBLimit = 4096

func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
		// 未知键只告警不报错：历史 config 里存在大量已退役键（cooldown.hard_credit /
		// pool.degrade_* / admin.enabled 等），报错会让老配置直接起不来。
		// 但静默忽略的代价是"配置漂移长期无人发现"——维护仓库的 config.json 曾同时
		// 带着 10 个不存在的键却没人察觉。折中：逐条 WARN，让漂移在日志里可见。
		warnUnknownKeys(raw)
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize（不做 env、不读文件）。
// 面板保存配置走这条路径：与 Load 完全同一套解析/校验逻辑，避免两处漂移。
//
// ⚠️ 契约（fuzz 目标 FuzzParseConfig 逼出来的）：`c` 必须是**已初始化**的配置
// （通常传 `Default()`）—— 本函数是"覆盖"语义，**不会**为未出现的字段补默认值。
// 传零值 `&Config{}` 会以 `server.max_body_mb: 0 非法` 失败（不是 panic，但信息容易误读）；
// 传 nil 现在等价于 `Default()`（旧实现会走到 `Unmarshal(nil)` 报一句很含糊的错）。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if c == nil {
		c = Default()
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	warnUnknownKeys(raw)
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON（等价于 Load 的文件分支，但不读环境变量）。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，双击即开免手工复制样例）。
// 值取自 Default()（含超时/熔断/签到排程等推荐值），api_key 用 crypto/rand 随机生成：
// 安全默认优于示例占位符（listen 绑定 0.0.0.0，空 key 会把网关裸暴露给局域网）。
// 返回生成的 key 供启动日志透出。已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。
func WriteDefault(path string) (string, error) {
	// 32 字节随机 → base64url 43 字符；前缀用项目约定的 sk-wb2a-（共 51 字符）。
	// 与线上既有密钥同形态（也便于 scripts/guard-secrets.sh 的项目专属规则生效）。
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-wb2a-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() 全合法，normalize 仅补齐 header/idle 超时的展示值
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判"不存在"，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

// warnUnknownKeys 把 raw JSON 中不被 Config 结构体识别的键逐条打成 WARN。
//
// 用途：JSON 反序列化默认忽略未知字段，导致拼错的键名、已退役的键、以及
// 从旧版本继承下来的配置**完全静默**——用户以为生效了，实际走的是默认值。
// 典型事故：`pool.expiring_soon` 早已被 critical/urgent_expire_hours 取代，
// 但旧 config 里留着它，运维照着调却毫无效果。
//
// 只做一层键名比对（不递归展开数组/对象内部），因为 Config 的嵌套只有一层
// 具名结构体，一层展开已能覆盖全部配置项；对 map 类型（如有）不做深入检查，
// 以免把用户数据误报为未知配置。
//
// 绝不返回错误、绝不改变解析结果：纯粹的观测性诊断，坏 key 不影响服务启动。
func warnUnknownKeys(raw []byte) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return // 顶层就不是对象：交给正式 Unmarshal 去报错
	}
	known := knownConfigKeys()
	var unknown []string
	for k, v := range top {
		if !known[k] {
			unknown = append(unknown, k)
			continue
		}
		// 已知的嵌套段：继续比对子键（Config 里只有一层具名结构体）。
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(v, &sub); err != nil {
			continue // 非对象（标量/数组）：键名已在上一层校验过，无需深入
		}
		for sk := range sub {
			if !known[k+"."+sk] {
				unknown = append(unknown, k+"."+sk)
			}
		}
	}
	if len(unknown) == 0 {
		return
	}
	sort.Strings(unknown) // 稳定输出便于 diff 与日志比对
	for _, k := range unknown {
		log.Printf("WARN: config: 未知配置键 %q 已被忽略（拼写错误或已退役？当前版本不会生效）", k)
	}
}

// knownConfigKeys 返回 Config 支持的键集合：顶层键 + "段.子键" 形式的一层子键。
// 用反射从结构体 json tag 推导，避免手写清单与结构体漂移（那正是本函数要解决的问题）。
// json:"-" 的字段（解析后中间值）与匿名字段不参与。
func knownConfigKeys() map[string]bool {
	known := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				continue
			}
			full := name
			if prefix != "" {
				full = prefix + "." + name
			}
			known[full] = true
			// 递归展开配置段。判据是"是否带 json tag"，而不是"是否匿名结构体"：
			// Config 的配置段既有内联匿名结构体（server/cooldown/pool...），
			// 也有具名类型（schedule 用的是 internal/config.Schedule），
			// 只认匿名会漏掉整个 schedule.* 而误报为未知键。
			// 反向安全性由"带 json tag"保证：time.Duration / time.Time 这类
			// 自定义类型没有带 tag 的字段，递归自然终止，不会把内部实现字段
			// 误当成配置键；map/slice 等非 struct 也不展开。
			if f.Type.Kind() == reflect.Struct && hasJSONTags(f.Type) {
				walk(f.Type, full)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return known
}

// hasJSONTags 报告结构体是否至少有一个字段带 json tag——用于判定它是不是"配置段"。
func hasJSONTags(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if tag := t.Field(i).Tag.Get("json"); tag != "" && tag != "-" {
			return true
		}
	}
	return false
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_UI_TRAY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.UI.Tray = &b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRE_URGENCY_WEIGHT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Pool.ExpireUrgencyWeight = f
		}
	}
	if v := os.Getenv("WB2A_EXPIRE_POWER"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Pool.ExpirePower = f
		}
	}
	if v := os.Getenv("WB2A_PACED_BREATHING_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PacedBreathingEnabled = &b
		}
	}
	if v := os.Getenv("WB2A_PACED_TIER2_CONSECUTIVE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Pool.PacedTier2ConsecutiveMax = n
		}
	}
}

func (c *Config) normalize() error {
	// 占位符 ⇒ 视为未设置（2026-10 首启体验）：避免"用户不知道密钥、控制台又进不去"的死锁。
	if c.APIKey == PlaceholderAPIKey {
		c.APIKey = ""
	}
	var err error
	// 出站 base URL 校验（2026-10-03 加，第十轮审计）：
	// 这四个字段决定"账号 access token 发往哪里"。此前**零校验** —— 面板（或任何能写
	// config 的人）把 chat_base 指向任意主机，网关就会把池内账号的 Bearer token 全量送过去。
	// 这里只做"不会把自己坑了"的底线校验（远端必须 https、禁 userinfo、禁非 http(s) 协议），
	// 不限制域名（用户指向自建反代是合法用法）。
	for _, f := range []struct{ name, val string }{
		{"global.chat_base", c.Global.ChatBase},
		{"global.billing_base", c.Global.BillingBase},
	} {
		if err := validateBaseURL(f.name, f.val); err != nil {
			return err
		}
	}
	// max_body_mb 非法（0/负数）直接报错：0 若被静默当成默认 8MB，用户以为"不限"，
	// 大请求又被静默 413——不如 fail fast 提示显式配大上限。
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（需为正整数，单位 MB）", c.Server.MaxBodyMB)
	}
	// 上限（fuzz 目标 FuzzParseConfig 逼出来的同族问题）：运行时是
	// `MaxBodyBytes = int64(MaxBodyMB) << 20`（cmd/server/main.go），超大值会**溢出 int64 变负**
	// （实测 `max_body_mb = 8796093022208` → -9223372036854775808；`1<<63-1` → -1048576），
	// 而 `io.LimitReader(r.Body, 负数)` 会立刻 EOF ⇒ **所有对话请求都变成"空 body"**，
	// 配置却看起来只是"把上限调大了"。4096 MB（4 GiB）远超任何真实用途。
	// 过渡钥与主钥相同属无效配置（会让"清空过渡钥"这一步失去意义）→ 直接清掉。
	if c.APIKeyPrevious == c.APIKey {
		c.APIKeyPrevious = ""
	}
	if c.Server.MaxBodyMB > maxBodyMBLimit {
		return fmt.Errorf("server.max_body_mb: %d 过大（上限 %d MB）—— 再大只会让 int64 左移溢出并把所有请求变成空 body",
			c.Server.MaxBodyMB, maxBodyMBLimit)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 负时长一律拒绝（fuzz 目标 FuzzParseConfig 发现：`"soft_rate":"-1h"` 能解析成功，
	// 于是软冷却的 until = now + (-1h) 落在**过去** → 冷却立即失效，而配置看起来"设过了"）。
	// 这类"配置写错但静默生效"的形态比报错危险得多，故 fail fast。
	if c.SoftRateDur < 0 {
		return fmt.Errorf("cooldown.soft_rate: %s 非法（不能为负）", c.Cooldown.SoftRate)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.SoftRateMaxDur < 0 {
		return fmt.Errorf("cooldown.soft_rate_max: %s 非法（不能为负）", c.Cooldown.SoftRateMax)
	}
	// 【P1-7】风控限制相关默认值（空 = 默认；非法值报错，与其它时长同口径）
	if c.Pool.RestrictionTTL == "" {
		c.Pool.RestrictionTTL = "2h"
	}
	if c.RestrictionTTLD, err = time.ParseDuration(c.Pool.RestrictionTTL); err != nil {
		return fmt.Errorf("pool.restriction_ttl: %w", err)
	}
	if c.Pool.RestrictionWaveWindow == "" {
		c.Pool.RestrictionWaveWindow = "15m"
	}
	if c.RestrictionWaveWinD, err = time.ParseDuration(c.Pool.RestrictionWaveWindow); err != nil {
		return fmt.Errorf("pool.restriction_wave_window: %w", err)
	}
	if c.Pool.RestrictionPause == "" {
		c.Pool.RestrictionPause = c.Pool.RestrictionWaveWindow
	}
	if c.RestrictionPauseD, err = time.ParseDuration(c.Pool.RestrictionPause); err != nil {
		return fmt.Errorf("pool.restriction_pause: %w", err)
	}
	// 出站身份预设置：未知取值直接报错（避免"配了但静默不生效"）。
	if _, ok := upstream.NormalizeIdentity(c.Upstream.Identity); !ok {
		return fmt.Errorf("upstream.identity: 未知取值 %q（可选：wb / vscode / cli，留空=默认自洽 WB）", c.Upstream.Identity)
	}
	if _, ok := upstream.NormalizePrivateData(c.Upstream.PrivateData); !ok {
		return fmt.Errorf("upstream.private_data: 未知取值 %q（可选：auto / true / false）", c.Upstream.PrivateData)
	}

	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownDur < 0 {
		return fmt.Errorf("pool.breaker_cooldown: %s 非法（不能为负）", c.Pool.BreakerCooldown)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.BreakerCooldownMaxD < 0 {
		return fmt.Errorf("pool.breaker_cooldown_max: %s 非法（不能为负）", c.Pool.BreakerCooldownMax)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionTTL < 0 {
		return fmt.Errorf("session_sticky.ttl: %s 非法（不能为负）", c.SessionSticky.TTL)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.SessionGCInterval < 0 {
		return fmt.Errorf("session_sticky.gc_interval: %s 非法（不能为负）", c.SessionSticky.GCInterval)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Pool.CriticalExpireHours <= 0 {
		c.Pool.CriticalExpireHours = 48.0
	}
	if c.Pool.ExpireUrgencyWeight <= 0 {
		c.Pool.ExpireUrgencyWeight = 15.0
	}
	if c.Pool.ExpirePower <= 0 {
		c.Pool.ExpirePower = 1.5
	}
	if c.Pool.PacedBreathingEnabled == nil {
		trueVal := true
		c.Pool.PacedBreathingEnabled = &trueVal
	}
	// 积分保底：0 合法（=关闭）⇒ 只钳负值，不能照搬本项目"<=0 落默认"的惯例。
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.PacedTier2ConsecutiveMax <= 0 {
		c.Pool.PacedTier2ConsecutiveMax = 3
	}
	// ui.tray 缺省 true（Windows 上开箱即有托盘）；显式 false 保留。
	if c.UI.Tray == nil {
		t := true
		c.UI.Tray = &t
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	// 余额后台刷新：启用时 minutes<=0 回落默认 5；关闭时 interval 保持 0（不启动）。
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "custom":
		c.Prompt.Mode = "custom"
	case "passthrough":
		c.Prompt.Mode = "passthrough"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		// Windows 路径容错：config.json 是 JSON，反斜杠本应写成 `\\`（或统一用 `/`），
		// 但真实用户（尤其从资源管理器地址栏复制）几乎总是直接粘 `C:\Users\...\p.md`。
		// 未转义时 JSON 解析会先失败（`\U` 非法转义序列）——那种情况此处已到不了；
		// 但 `C:\\Users` 正确转义后 Go 在 Windows 上本就能识别反斜杠，故此处只做
		// **统一成 `/`** 的归一：让后续日志/报错里的路径形态稳定，且消除
		// "同一路径两种写法"导致的 filepath 比较歧义。非 Windows 平台不动——
		// 反斜杠在 Unix 是合法文件名字符，替换会破坏真实文件名。
		if runtime.GOOS == "windows" {
			c.Prompt.File = strings.ReplaceAll(c.Prompt.File, `\`, "/")
		}
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateBaseURL 校验出站 base URL 的底线安全属性。
//
// 为什么（第十轮审计）：这些字段指向"账号凭证要发给谁"。空白 = 回落内置默认（合法）；
// 显式配置时至少要保证：协议是 http(s)、有主机名、不带 userinfo、**远端必须 https** ——
// 否则 http 明文会把 access token 暴露给同链路任何人（本机回环除外，方便本地抓包调试）。
func validateBaseURL(field, raw string) error {
	raw = strings.TrimSpace(raw) // 粘贴常带前后空白：先 trim 再解析，否则报"URL 不能含冒号"这类误导错误
	if raw == "" {
		return nil // 空 = 回落内置默认（见 upstream.defaultGlobalBase / ChatBaseCN）
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: 不是合法 URL: %v", field, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%s: 只支持 http/https（实得 %q）", field, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%s: 缺少主机名（%q）", field, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%s: URL 不允许携带用户名/密码", field)
	}
	if u.Scheme == "http" && !loopbackHostName(u.Hostname()) {
		return fmt.Errorf("%s: 远端地址必须用 https —— http 会把账号 access token 明文发出（仅本机回环允许 http）", field)
	}
	return nil
}

// loopbackHostName 判断主机名是否指向本机回环（localhost / 127.0.0.0/8 / ::1）。
func loopbackHostName(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ParseServerReadTimeout 解析 server.read_timeout（上游 15a6fc9，issue #100）：
// "" ⇒ 缺省 300s；"0" ⇒ 0（不限制，合法）；负值/非法一律 fail fast —— 静默钳 0 会把保护
// 悄悄关掉，而"读超时被关掉"只会在生产上以慢速 body 拖死连接的形式暴露。
func ParseServerReadTimeout(v string) (time.Duration, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		t = "300s"
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, fmt.Errorf("server.read_timeout: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("server.read_timeout: 负时长 %q 无意义", v)
	}
	return d, nil
}
