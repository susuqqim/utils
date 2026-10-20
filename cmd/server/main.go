// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"workbuddy2api/internal/cstime"
	"workbuddy2api/internal/gosafe"
	"workbuddy2api/internal/reqlog"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/autostart"
	"workbuddy2api/internal/livecfg"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/tray"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const appVersion = "1.11.11-panel"

// restartHidden 为真时（托盘模式），重启子进程用 CREATE_NO_WINDOW 拉起，
// 避免新控制台闪窗；仅 Windows 生效（见 restart_windows.go）。
var restartHidden bool

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
// shouldWarmOnHotEnable 报告"面板热开保底"是否需要补一次倍率预热：仅 0 → 正值这一次跳变
// 需要（保底从关闭变开启，而预热只在启动时跑过一次、当时保底通常为 0 ⇒ 倍率表为空）。
// 抽成纯函数是为了能被单测钉死（审查 E 的 P1-1：早期版本先写后判，判定恒假）。
func shouldWarmOnHotEnable(oldFloor, newFloor int64) bool {
	return oldFloor <= 0 && newFloor > 0
}

// warmModelRates 启动预热各域模型倍率表（供积分保底的目录兜底）。异步、不阻塞监听：
// 2s 错峰（本地启动阶段已有 syncAllResources 逐账号探测，避免叠加打上游）；
// 保底未开则直接返回（零上游开销）；单域失败只 WARN；ctx 取消即放弃剩余域。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	if p.CreditFloor() <= 0 {
		return // 倍率表无人消费（保底关闭）：不发任何探测请求
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty catalog")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

func main() {
	// autostart 子命令：由 wb2api.cmd 直接调用（见 internal/autostart 与路线图 §N），
	// 不再经 cmd -> powershell -ExecutionPolicy Bypass -> *.ps1 的持久化链路——
	// 该链路会命中杀软"启动项防护"启发式（火绒 fp_name_touch_startup_malware）。
	if len(os.Args) > 1 && strings.EqualFold(os.Args[1], "autostart") {
		os.Exit(runAutostartCmd(os.Args[2:]))
	}

	// [2026-10] 更新自检（用户需求：装了新版要有提示）。
	// 必须在 main() 本体：曾误放进 restartSvc 闭包 ⇒ 只有"点重启"才跑，正常启动永不执行（QA 抓到）。
	// 30s 让监听/托盘就绪；有新版由 tray.Notify 弹 Windows 通知 —— 全程进程内，不 spawn cmd。
	go func() {
		if !tray.Supported() {
			return // 无托盘（Linux/headless）时不出站、不通知：面板按钮仍可手动检查
		}
		time.Sleep(30 * time.Second)
		checkUpdateAndNotify(appVersion)
	}()

	cfgPath := flag.String("config", "config.json", "path to config json")
	noTray := flag.Bool("no-tray", false, "禁用系统托盘图标（Windows；等价于 config ui.tray=false）")
	flag.Parse()

	// 启动即把工作目录切到 config 所在目录（路线图 §O）。
	// 开机自启（Windows HKCU Run / macOS launchd）**不设置工作目录**，而 config 里的
	// auth_dir/state_file 默认是相对路径（./auths、./data/state.json）→ 按 CWD 解析会加载
	// 0 个账号，且 /healthz 仍报 healthy（静默故障）；派生子进程按相对路径读 config.json
	// 也会失败。手动启动（wb2api.cmd 有 `cd /d "%DIR%"`）、systemd（WorkingDirectory=）与
	// crontab（cd）本来都保证了工作目录，在进程内自愈可一次修好所有平台与子进程。
	absCfg, chdirTo := resolveConfigPath(*cfgPath)
	if absCfg != "" {
		*cfgPath = absCfg
	}
	if chdirTo != "" {
		if err := os.Chdir(chdirTo); err != nil {
			log.Printf("WARN: chdir 到 config 目录 %q 失败: %v（相对路径将按当前工作目录解析）", chdirTo, err)
		}
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		isNotExist := errors.Is(err, os.ErrNotExist) || os.IsNotExist(err)
		// 配置文件不存在时：若未指定自定义 -config（即缺省的 config.json），
		// 先尝试回退检查标准数据目录（WB2API_HOME / %USERPROFILE%\.workbuddy2api / ~/.workbuddy2api），
		// 避免裸双击或在非数据目录直接执行 core 时因 CWD 无 config 而静默加载 0 账号。
		if isNotExist && (*cfgPath == "config.json" || filepath.Base(*cfgPath) == "config.json") {
			if fallbackCfg, fallbackDir := findDefaultConfig(); fallbackCfg != "" {
				log.Printf("当前目录未找到 config.json，自动回退使用数据目录配置: %s", fallbackCfg)
				*cfgPath = fallbackCfg
				if fallbackDir != "" {
					_ = os.Chdir(fallbackDir)
				}
				cfg, err = Load(*cfgPath)
			}
		}
		if err != nil {
			// 仍不存在时给一次机会用纯默认 + env
			if errors.Is(err, os.ErrNotExist) || os.IsNotExist(err) {
				log.Printf("config %s not found, using defaults+env", *cfgPath)
				cfg, err = Load("")
			}
			if err != nil {
				log.Fatalf("load config: %v", err)
			}
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	// Phase 3 加权调度与分块策略（动态三档自适应分块、交棒让渡、安全期负重平衡、临期梯队与断崖保护）
	p.SetCreditFloor(cfg.Pool.CreditFloor)
	p.SetSchedulePolicy(pool.SchedulePolicy{
		ChunkQuotaCredits:        cfg.Pool.ChunkQuotaCredits,
		CriticalExpireHours:      cfg.Pool.CriticalExpireHours,
		UrgentExpireHours:        cfg.Pool.UrgentExpireHours,
		NearExpireDays:           cfg.Pool.NearExpireDays,
		SafeReserveDays:          cfg.Pool.SafeReserveDays,
		LowCreditCutoff:          cfg.Pool.LowCreditCutoff,
		ExpireUrgencyWeight:      cfg.Pool.ExpireUrgencyWeight,
		ExpirePower:              cfg.Pool.ExpirePower,
		AdaptiveChunking:         cfg.Pool.AdaptiveChunking == nil || *cfg.Pool.AdaptiveChunking,
		TierUnknownQuota:         cfg.Pool.TierUnknownQuota,
		TierUnknownMaxReqs:       cfg.Pool.TierUnknownMaxReqs,
		TierCheapCostCutoff:      cfg.Pool.TierCheapCostCutoff,
		TierFlagshipCostCutoff:   cfg.Pool.TierFlagshipCostCutoff,
		TierCheapQuota:           cfg.Pool.TierCheapQuota,
		TierCheapMaxReqs:         cfg.Pool.TierCheapMaxReqs,
		TierNormalQuota:          cfg.Pool.TierNormalQuota,
		TierNormalMaxReqs:        cfg.Pool.TierNormalMaxReqs,
		TierFlagshipQuota:        cfg.Pool.TierFlagshipQuota,
		TierFlagshipMaxReqs:      cfg.Pool.TierFlagshipMaxReqs,
		PacedBreathingEnabled:    cfg.Pool.PacedBreathingEnabled == nil || *cfg.Pool.PacedBreathingEnabled,
		PacedTier2ConsecutiveMax: cfg.Pool.PacedTier2ConsecutiveMax,
	})

	// 启动时立即打印每个账号当前画像（积分、消耗、到期、状态）
	logAccountsSummary(p, "启动初始化账号画像")

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 积分保底的目录倍率兜底：pool 不依赖 upstream 包，用闭包注入（每次调用读实时快照）。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// tool_call 元数据兼容开关（见 config features.tool_call_meta_repeat 注释）。
	up.ToolCallMetaRepeat = cfg.Features.ToolCallMetaRepeat
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                   p,
		Upstream:               up,
		CheckinHours:           cfg.Schedule.CheckinHours,
		GrowthHours:            cfg.Schedule.GrowthHours,
		TravelHours:            cfg.Schedule.TravelHours,
		ActivityHours:          cfg.Schedule.ActivityHours,
		KeepaliveHours:         cfg.Schedule.KeepaliveHours,
		CatHours:               cfg.Schedule.CatHours,
		BlackcatHours:          cfg.Schedule.BlackcatHours,
		ActivityReportCount:    cfg.Schedule.ActivityReportCount,
		CheckinDisabled:        !cfg.Schedule.CheckinEnabled,
		GrowthDisabled:         !cfg.Schedule.GrowthEnabled,
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		EnterpriseGate:         cfg.EnterpriseGate,
		TravelDisabled:         !cfg.Schedule.TravelEnabled,
		ActivityDisabled:       !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:      !cfg.Schedule.KeepaliveEnabled,
		CatDisabled:            !cfg.Schedule.CatEnabled,
		BlackcatDisabled:       !cfg.Schedule.BlackcatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.GrowthEnabled {
		log.Printf("成长任务排程已禁用（schedule.growth_enabled=false）")
	} else {
		log.Printf("成长任务排程已启用：%v 点（到点自动扫描+执行待办）", cfg.Schedule.GrowthHours)
	}
	if !cfg.Schedule.CatEnabled && !cfg.Schedule.BlackcatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		APIKeyPrevious:       cfg.APIKeyPrevious,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	var (
		serverStop   func()
		httpSrv      *http.Server
		isRestarting atomic.Bool
		restartDone  = make(chan struct{})
	)

	// 系统托盘开关（Windows 原生；默认开启，config ui.tray=false 或启动加 -no-tray 可关）。
	// 提前求值：restartSvc 里要据此决定重启子进程要不要控制台。
	trayOn := trayEnabled(cfg, *noTray)

	// 系统生命周期回调：管理面板与系统托盘共用同一份实现，天然继承既有语义
	// （重启幂等、优雅落盘、Docker 特判、serverStop 触发退出）。
	restartSvc := func() error {
		if isRestarting.Swap(true) {
			return nil
		}
		log.Printf("[system] 收到重启指令，准备重启网关服务...")
		go gosafe.Do("server restart drain", func() {
			defer close(restartDone)

			p.Flush()
			rec.Stop()
			if httpSrv != nil {
				sCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
				defer c()
				_ = httpSrv.Shutdown(sCtx)
			}
			time.Sleep(600 * time.Millisecond)

			// 先摘掉本进程的托盘图标并等消息循环退出，再拉起新进程——否则两个进程的
			// 图标会短暂并存（旧图标要等 WM_CLOSE 才移除，而它可能在进程退出前都轮不到）。
			if trayOn {
				tray.Stop()
			}

			if !isDocker() {
				exe, err := os.Executable()
				if err == nil {
					wd, _ := os.Getwd()
					// 按**子进程**的实际配置决定控制台策略：若把 ui.tray 改成了 false 再重启，
					// 子进程不会有托盘，此时必须给它一个新控制台，否则它既无托盘也无窗口，
					// 变成只能靠面板地址/taskkill 操作的"隐身进程"。
					restartHidden = childWillShowTray(*cfgPath, *noTray, cfg)
					if err := launchRestart(exe, os.Args[1:], wd); err != nil {
						log.Printf("[system] 启动重启新进程失败: %v", err)
					} else {
						log.Printf("[system] 重启新进程已启动: %s", exe)
					}
				}
			}
			if serverStop != nil {
				serverStop()
			}
		})
		return nil
	}
	shutdownSvc := func() error {
		log.Printf("[system] 收到关闭指令，准备停止网关服务...")
		if isDocker() {
			// 在 Docker (restart: unless-stopped) 环境下，主进程直接退出会被
			// 容器运行时判定为异常/终止并自动拉起，导致无法停止。
			// 因此优雅关闭监听后进入休眠挂起状态。
			go gosafe.Do("server shutdown drain", func() {
				p.Flush()
				rec.Stop()
				if httpSrv != nil {
					sCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
					defer c()
					_ = httpSrv.Shutdown(sCtx)
				}
				log.Printf("[system] 网关服务已停止监听（Docker 容器运行中；如需彻底停止容器请在宿主机执行 docker compose stop）")
				select {}
			})
			return nil
		}
		if serverStop != nil {
			serverStop()
		}
		return nil
	}

	pn := panel.New(panel.Config{
		Pool:           p,
		Usage:          rec,
		Upstream:       up,
		Scheduler:      sch,
		AuthDir:        cfg.AuthDir,
		APIKey:         cfg.APIKey,
		APIKeyPrevious: cfg.APIKeyPrevious,
		RedisMode:      redisMode,
		StickyCount:    sessCount,
		Version:        appVersion,
		Live:           live,
		ProbeFile:      stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath:     *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch)
		},
		Restart:  restartSvc,
		Shutdown: shutdownSvc,
		// 单号签到与排程全量签到共用一把锁：面板连点/手动与定时撞车时，
		// 后到的请求得到 409 而不是并发打同一账号（第十轮审计 F5）。
		TryCheckinLock: sch.TryCheckinLock,
		UnlockCheckin:  sch.UnlockCheckin,
	})
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 日志镜像到面板环：**环放在第一位**，且控制台 writer 包一层"吞掉写错误"。
	// 为什么（2026-10-05 自查发现的回归）：托盘模式下 FreeConsole 之后 os.Stderr/os.Stdout
	// 已失效，而 io.MultiWriter 遇到第一个 writer 报错就**短路**——若控制台排在前面，
	// 面板日志环会从此收不到任何日志（表现为"环里只剩启动时那一条"）。
	log.SetOutput(io.MultiWriter(pn.Logs(), ignoreWriteErr(os.Stderr)))
	server.SetChatLogOutput(io.MultiWriter(pn.Logs(), ignoreWriteErr(os.Stdout)))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		HiddenModels: cfg.HiddenModels,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		PromptMode:   cfg.Prompt.Mode,
		// 【P1-7】上游风控限制防连坐
		RestrictedRotateMax:   cfg.Pool.RestrictedRotateMax,
		RestrictionTTL:        cfg.RestrictionTTLD,
		RestrictionWaveWindow: cfg.RestrictionWaveWinD,
		RestrictionWaveMin:    cfg.Pool.RestrictionWaveMin,
		RestrictionPause:      cfg.RestrictionPauseD,
		PromptText:            cfg.PromptText,
		MaxBodyBytes:          int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverStop = stop
	go gosafe.Do("scheduler run", func() { sch.Run(ctx) })
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	// 启动即预热模型积分倍率表（上游 a4557dc）：倍率表只在 FetchModels /
	// FetchGlobalModelInfos 成功时填充，两者均懒触发 —— 不预热的话，重启后到首次
	// /v1/models 或面板模型页被访问之前 ModelRate 恒空，保底会把触底号当"收费未知"
	// 放行并打穿（上游实测：重启后约 2 分钟，97 分的号打收费模型归零）。
	go gosafe.Do("warm model rates", func() { warmModelRates(ctx, up, p) })
	// 资产画像同步（启动后一次 + 每小时一次）受 schedule.resource_sync_enabled 控制：
	// 它是唯一此前**无法关闭**的定期上游调用，测试/离线实例需要零上游消耗时必须能关（第十轮审计 F1）。
	if cfg.Schedule.ResourceSyncEnabled {
		go gosafe.Do("resource sync", func() { syncAllResources(ctx, p, up, cfg.AuthDir) })
	} else {
		log.Printf("resource sync: 已按配置关闭（schedule.resource_sync_enabled=false，不会打上游）")
	}

	// 系统托盘（Windows 原生）与网关**同进程**：wb2api stop 的 taskkill /IM wb2api-core.exe
	// 会连同托盘一起结束，不会留下孤儿托盘进程。开关已在上面求值为 trayOn；
	// 实际启动在下面 httpSrv 就绪之后（避免托盘线程读到尚未赋值的 httpSrv）。
	defer tray.Stop()

	// 开机即时自动巡检：服务就绪后异步自动执行一次签到与猫猫旅行（无需苦等下一个整点时段 09:00/21:00）
	go gosafe.Do("startup checkin", func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3500 * time.Millisecond):
		}
		if cfg.Schedule.CheckinEnabled {
			log.Printf("[scheduler] 触发启动初始自动签到...")
			sch.RunCheckinNow()
		}
		if cfg.Schedule.TravelEnabled {
			log.Printf("[scheduler] 触发启动初始猫猫旅行巡检...")
			sch.RunTravelNow()
		}
	})

	// 每日 00:00 CST 自然日跨日刷新：自动重置今日用量统计并通知持久化
	go gosafe.Do("cst midnight reset", func() {
		for {
			now := time.Now().In(cstime.Zone) // 单一口径：见 internal/cstime
			nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 1, 0, now.Location())
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(nextMidnight)):
				// 迭代级兜底：顶层 recover 会让跨日重置循环结束 → 今日用量永远不再归零。
				gosafe.Loop("cst midnight reset", func() {
					p.ResetDailyUsageIfExpired()
					log.Printf("[account] 00:00 CST 自然日跨日刷新：今日积分与 Token 消耗统计已自动重置归零")
				})
			}
		}
	})

	regReadTimeout, err := ParseServerReadTimeout(cfg.Server.ReadTimeout)
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	} // 请求指标采集（S3）：默认关闭；开启后只包网关 handler，采集 /v1/* 的结构化指标，
	// 走 reqlog 脱敏（IP /24、UA 120、uid 8 位），**不落正文**；归档默认 7 天 / ≤50MB。
	var rootHandler http.Handler = h
	if cfg.ReqLog.Enabled {
		rlRing := reqlog.NewRing(500)

		var arch *reqlog.Archive
		if cfg.ReqLog.Dir != "" {
			arch = reqlog.NewArchive(cfg.ReqLog.Dir, time.Duration(cfg.ReqLog.RetainDays)*24*time.Hour, int64(cfg.ReqLog.MaxMB)<<20)
		}
		rootHandler = server.ReqLogMiddleware(rootHandler, &server.ReqLogSink{Ring: rlRing, Archive: arch})
		pn.SetReqLog(rlRing, arch) // 面板只读视图数据源（S3）
		log.Printf("请求指标采集已启用：环 500 · 归档目录=%q · 保留 %d 天 · ≤%dMB", cfg.ReqLog.Dir, cfg.ReqLog.RetainDays, cfg.ReqLog.MaxMB)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           rootHandler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       regReadTimeout,
		IdleTimeout:       120 * time.Second,
	}
	httpSrv = srv

	// 托盘在 httpSrv 就绪后启动：重启回调会读 httpSrv，避免未同步并发读。
	// 注意 restartHidden **不在这里**赋值：它必须在每次重启时按"子进程会不会有托盘"
	// 重新计算（见 restartSvc），否则 ui.tray 从 true 改成 false 后重启出来的子进程
	// 会被 CREATE_NO_WINDOW 拉起——既无托盘也无控制台。
	if trayOn {
		exe, _ := os.Executable()
		wd, _ := os.Getwd()
		lp := panelListenPath(cfg.Listen) // ":17963"
		if err := tray.Start(tray.Options{
			PanelURL:    "http://127.0.0.1" + lp + "/panel/",
			HealthURL:   "http://127.0.0.1" + lp + "/healthz",
			Version:     appVersion,
			HideConsole: true,
			CorePath:    exe,
			DataDir:     wd,
			OnRestart:   restartSvc,
			OnShutdown:  shutdownSvc,
			StatusProvider: func() tray.StatusInfo {
				if p == nil {
					return tray.StatusInfo{Online: false}
				}
				tot, hly, clg, dis, _ := p.CountsDetailed()
				cnTot, cnHly, _, _, _ := p.CountsDetailedForRealm("cn")
				glTot, glHly, _, _, _ := p.CountsDetailedForRealm("global")
				dailyCredits, _, _, _ := p.UsageTotals()

				var totalRemain int64
				var urgent48 int64
				for _, st := range p.List() {
					if !st.Disabled {
						totalRemain += st.Credits
						urgent48 += st.Urgent48hRemain
					}
				}
				return tray.StatusInfo{
					Online:        true,
					Healthy:       hly,
					Total:         tot,
					Cooling:       clg,
					Disabled:      dis,
					CNHealthy:     cnHly,
					CNTotal:       cnTot,
					GlobalHealthy: glHly,
					GlobalTotal:   glTot,
					TotalCredits:  totalRemain,
					DailyCredits:  dailyCredits,
					Urgent48h:     urgent48,
				}
			},
		}); err != nil {
			log.Printf("WARN: 系统托盘启动失败（不影响网关运行，控制台保持可见）: %v", err)
		} else {
			log.Printf("[tray] 系统托盘已启动（左键打开面板；右键菜单：面板/重启/开机自启/退出）")
		}
	}

	// 注意：**刻意不加** goroutine 顶层 recover。这里的预期语义就是"退出"：
	// 若 Flush 抛 panic 又被吞掉，srv.Shutdown 永不执行 → Ctrl-C/托盘退出看起来毫无反应，
	// 只能人工强杀（而崩溃至少让进程真的结束）。崩溃在此是可接受的失败模式。
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	if isRestarting.Load() {
		<-restartDone
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("bye")
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 2b) 可见性留痕（第十轮审计）：出站 base URL 决定"账号 access token 发往哪里"。
	// 面板可以改这两个字段，改了却静默生效是危险行为 —— 变更必须留一条 WARN。
	if oldCfg, oerr := ParseConfig(oldRaw); oerr == nil {
		for _, f := range []struct{ name, oldV, newV string }{
			{"global.chat_base", oldCfg.Global.ChatBase, newCfg.Global.ChatBase},
			{"global.billing_base", oldCfg.Global.BillingBase, newCfg.Global.BillingBase},
		} {
			if f.oldV != f.newV {
				log.Printf("WARN: 配置变更 %s: %q → %q（此后账号 access token 会发往新地址，请确认这是你要的）",
					f.name, f.oldV, f.newV)
			}
		}
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// 单文件 bind mount 或跨文件系统时 rename 会失败（EBUSY / EXDEV / EPERM 等），回退为直接覆盖写原文件
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			if errors.Is(openErr, syscall.EROFS) || strings.Contains(openErr.Error(), "read-only file system") {
				return nil, fmt.Errorf("配置文件为只读挂载（read-only），无法在线保存。请检查 docker-compose.yml 中 ./config.json 的挂载配置，移除尾部的 ':ro' 后重启容器即可支持管理面板在线修改")
			}
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		APIKeyPrevious:       newCfg.APIKeyPrevious,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
	})
	up.ToolCallMetaRepeat = newCfg.Features.ToolCallMetaRepeat // 热重载同样生效
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	up.ChatBaseGlobal = newCfg.Global.ChatBase
	up.BillingBaseGlobal = newCfg.Global.BillingBase
	up.GlobalEnabled = newCfg.Global.Enabled
	auth.SetGlobalEnabled(newCfg.Global.Enabled)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	// 先取旧值再写：热开保底的补预热判据必须用**旧值**（否则 Set 之后
	// `p.CreditFloor() <= 0 && newCfg… > 0` 恒假 —— 死守卫，审查 E 的 P1-1）。
	oldFloor := p.CreditFloor()
	p.SetCreditFloor(newCfg.Pool.CreditFloor)
	p.SetSchedulePolicy(pool.SchedulePolicy{
		ChunkQuotaCredits:        newCfg.Pool.ChunkQuotaCredits,
		CriticalExpireHours:      newCfg.Pool.CriticalExpireHours,
		UrgentExpireHours:        newCfg.Pool.UrgentExpireHours,
		NearExpireDays:           newCfg.Pool.NearExpireDays,
		SafeReserveDays:          newCfg.Pool.SafeReserveDays,
		LowCreditCutoff:          newCfg.Pool.LowCreditCutoff,
		ExpireUrgencyWeight:      newCfg.Pool.ExpireUrgencyWeight,
		ExpirePower:              newCfg.Pool.ExpirePower,
		AdaptiveChunking:         newCfg.Pool.AdaptiveChunking == nil || *newCfg.Pool.AdaptiveChunking,
		TierUnknownQuota:         newCfg.Pool.TierUnknownQuota,
		TierUnknownMaxReqs:       newCfg.Pool.TierUnknownMaxReqs,
		TierCheapCostCutoff:      newCfg.Pool.TierCheapCostCutoff,
		TierFlagshipCostCutoff:   newCfg.Pool.TierFlagshipCostCutoff,
		TierCheapQuota:           newCfg.Pool.TierCheapQuota,
		TierCheapMaxReqs:         newCfg.Pool.TierCheapMaxReqs,
		TierNormalQuota:          newCfg.Pool.TierNormalQuota,
		TierNormalMaxReqs:        newCfg.Pool.TierNormalMaxReqs,
		TierFlagshipQuota:        newCfg.Pool.TierFlagshipQuota,
		TierFlagshipMaxReqs:      newCfg.Pool.TierFlagshipMaxReqs,
		PacedBreathingEnabled:    newCfg.Pool.PacedBreathingEnabled == nil || *newCfg.Pool.PacedBreathingEnabled,
		PacedTier2ConsecutiveMax: newCfg.Pool.PacedTier2ConsecutiveMax,
	})
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours,
		newCfg.Schedule.CatHours, newCfg.Schedule.BlackcatHours, newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled,
		!newCfg.Schedule.CatEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	sch.SetEnterpriseGate(newCfg.EnterpriseGate)
	// 热开积分保底（0 → 正值）时补一次倍率预热：预热原本只在启动时跑，而启动时保底通常
	// 关闭（默认 0）⇒ 热开会让"倍率表为空"的空窗期重现（此窗口内触底号打收费模型照旧
	// 归零）。审查 D 的 P2-7。
	if shouldWarmOnHotEnable(oldFloor, newCfg.Pool.CreditFloor) {
		bg := context.Background()
		go gosafe.Do("warm model rates (hot enable)", func() { warmModelRates(bg, up, p) })
	}

	return restartRequiredFields(newCfg), nil
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
func restartRequiredFields(c *Config) []string {
	var out []string
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	// 托盘在启动时创建，无法运行中热插拔，故改 ui.tray 需重启进程生效。
	out = append(out, "ui.tray")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	// 资产同步的 goroutine 只在启动时创建（关掉它意味着"不再打上游"，热改需要停掉
	// 正在跑的 ticker），故列入需重启项 —— 否则用户在面板改了却静默无效（第十一轮审计 F4）。
	out = append(out, "schedule.resource_sync_enabled")
	// server.read_timeout 只在启动时读取（http.Server 构造期）⇒ 改动需重启（与上面几个
	// 无条件列出的启动期字段同一体例）。
	out = append(out, "server.read_timeout")

	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// syncAllResources 资产数据感知：异步从上游获取各账号的实时余额与最早到期时间。
func syncAllResources(ctx context.Context, p *pool.Pool, up *upstream.Client, authDir string) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(1500 * time.Millisecond):
	}

	doSync := func() {
		auths, err := auth.LoadDir(authDir)
		if err != nil || len(auths) == 0 {
			return
		}
		log.Printf("[pool] 正在执行资产数据感知同步 (账号数=%d)...", len(auths))
		for _, a := range auths {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if a.AccessToken == "" {
				continue
			}
			detail, err := up.ResourceDetail(a)
			if err != nil {
				log.Printf("WARN: [pool] 账号 %s 资产同步失败: %v", logfmt.UID8(a.UID), err)
				continue
			}
			var pkgs []pool.PackageItem
			for _, pkg := range detail.PackageList {
				pkgs = append(pkgs, pool.PackageItem{
					Remain: pkg.Remain,
					Expire: pkg.Expire,
				})
			}
			p.UpdateResourceDetail(a.UID, detail.Remain, detail.EarliestExpire, detail.Urgent24hRemain, detail.Urgent72hRemain, pkgs...)
			expStr := "无到期日"
			if !detail.EarliestExpire.IsZero() {
				days := time.Until(detail.EarliestExpire).Hours() / 24.0
				expStr = fmt.Sprintf("%s (约%.1f天后)", detail.EarliestExpire.Format("2006-01-02 15:04"), days)
			}
			pkgInfo := ""
			if len(pkgs) > 1 {
				pkgInfo = fmt.Sprintf(" (含%d个套餐包)", len(pkgs))
			}
			log.Printf("[pool] 账号 %s 资产已感知: 剩余=%d%s, 48h临期=%d, 72h临期=%d, 总量=%d, 最早到期=%s",
				logfmt.UID8(a.UID), detail.Remain, pkgInfo, detail.Urgent24hRemain, detail.Urgent72hRemain, detail.Size, expStr)
			time.Sleep(300 * time.Millisecond) // 防频控
		}
		p.Flush()
		logAccountsSummary(p, "上游资产感知已就绪")
	}

	// 1. 启动时执行一次
	doSync()

	// 2. 定时同步（每 1 小时一次）
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 迭代级兜底：顶层 recover 会让本循环结束 → 资产画像永久停止同步（无报错）。
			gosafe.Loop("resource sync tick", func() { doSync() })
		}
	}
}

// logAccountsSummary 输出账号池格式化概览画像（用于启动阶段与上游资产同步）。
func logAccountsSummary(p *pool.Pool, title string) {
	list := p.List()
	if len(list) == 0 {
		log.Printf("[account] === %s: 账号池为空 (未发现凭证) ===", title)
		return
	}
	log.Printf("[account] ==================== %s (共 %d 个账号) ====================", title, len(list))
	var sumDailyCredits, sumTotalCredits float64
	var sumDailyTokens, sumTotalTokens int64
	for i, a := range list {
		sumDailyCredits += a.DailyCreditsUsed
		sumTotalCredits += a.CreditsUsed
		sumDailyTokens += a.DailyTokensUsed
		sumTotalTokens += a.TotalTokens

		statusStr := "正常可用"
		if a.Disabled {
			statusStr = fmt.Sprintf("已禁用(%s)", a.DisabledReason)
		} else if a.Paused {
			statusStr = "已暂停选号" // 暂停选号 ≠ 禁用：保号任务照常，可随时恢复
		} else if a.Cooling {
			statusStr = fmt.Sprintf("冷却中(%ds)", a.CoolRemaining)
		}
		expStr := "无到期日"
		if !a.EarliestExpire.IsZero() {
			expStr = fmt.Sprintf("%s (约%.1f天后)", a.EarliestExpire.Format("2006-01-02 15:04"), a.DaysUntilExpire)
		}
		nick := a.Nickname
		if nick == "" {
			nick = "未知"
		}
		realm := strings.ToUpper(a.Realm)
		if realm == "" {
			realm = "CN"
		}
		log.Printf("[account] #%d [%s] 昵称: %-10s (uid=%s) | 状态: %s | 剩余积分: %d | 今日消耗: %.2f | 累计已用: %.2f | 48h临期: %d | 72h临期: %d | 到期: %s",
			i+1, realm, nick, logfmt.UID8(a.UID), statusStr, a.Credits, a.DailyCreditsUsed, a.CreditsUsed, a.Urgent24hRemain, a.Urgent72hRemain, expStr)
	}
	log.Printf("[account] 消耗统计汇总: 今日已消耗 %.2f credits (%d tokens) | 历史累计消耗 %.2f credits (%d tokens)",
		sumDailyCredits, sumDailyTokens, sumTotalCredits, sumTotalTokens)
	log.Printf("[account] ======================================================================")
}

// trayEnabled 报告是否应启用系统托盘：仅 Windows、非 Docker、配置未显式关闭、且未传 -no-tray。
func trayEnabled(cfg *Config, noTray bool) bool {
	if noTray || isDocker() || runtime.GOOS != "windows" {
		return false
	}
	return cfg.UI.Tray == nil || *cfg.UI.Tray
}

// childWillShowTray 预测"重启出来的子进程会不会有托盘"：按**磁盘上最新的** config 重新判定
// （用户可能刚在面板把 ui.tray 改成 false，然后点了重启）。
// 只有子进程确实会有托盘时，才允许用 CREATE_NO_WINDOW 拉起它；否则必须给它一个控制台，
// 不然它会变成既无托盘又无窗口的"隐身进程"
// （只能靠已知的面板地址或 taskkill 操作）。读配置失败则退回当前内存配置。
func childWillShowTray(cfgPath string, noTray bool, cur *Config) bool {
	c := cur
	if cfg2, err := Load(cfgPath); err == nil {
		c = cfg2
	}
	return trayEnabled(c, noTray)
}

// isDocker 判断当前是否运行在 Docker 容器内部。
func isDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("WB2API_CONTAINER") != "" || os.Getenv("DOCKER_CONTAINER") != "" {
		return true
	}
	return false
}

// runAutostartCmd 处理 `wb2api-core.exe autostart on|off|status [--startup-folder] [--dir <data-dir>]`，
// 返回进程退出码。默认只写 HKCU\...\Run（免提权、免额外脚本）；--startup-folder
// 才启用启动文件夹 .cmd 兜底（该形态是杀软重点监控对象，故不默认使用）。
func runAutostartCmd(args []string) int {
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = strings.ToLower(args[0])
		args = args[1:]
	}
	useStartupFolder := false
	customDir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch strings.ToLower(a) {
		case "--startup-folder", "-startup-folder":
			useStartupFolder = true
		case "--dir", "-dir", "-d":
			if i+1 < len(args) {
				customDir = args[i+1]
				i++
			}
		default:
			if strings.HasPrefix(strings.ToLower(a), "--dir=") {
				customDir = a[6:]
			} else if strings.HasPrefix(strings.ToLower(a), "-dir=") {
				customDir = a[5:]
			}
		}
	}
	switch action {
	case "on", "enable":
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] resolve exe path: %v\n", err)
			return 1
		}
		var dir string
		if customDir != "" {
			dir = customDir
		} else if _, fallbackDir := findDefaultConfig(); fallbackDir != "" {
			// 若当前工作目录下无 config.json 但标准数据目录有，优先使用标准数据目录
			if _, err := os.Stat("config.json"); err != nil {
				dir = fallbackDir
			} else {
				dir, _ = os.Getwd()
			}
		} else {
			dir, err = os.Getwd()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR] resolve data dir: %v\n", err)
				return 1
			}
		}
		absDir, err := filepath.Abs(strings.Trim(strings.TrimSpace(dir), "\"'"))
		if err == nil {
			dir = absDir
		}
		cfgFile := filepath.Join(dir, "config.json")
		if _, err := os.Stat(cfgFile); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] config.json 在 %s 未找到，自启启动时将使用默认配置或环境变量\n", cfgFile)
		}
		info, err := autostart.Enable(exe, dir, useStartupFolder)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			fmt.Fprintln(os.Stderr, "[HINT] if blocked by antivirus, add the workbuddy2api folder to its trust zone,")
			fmt.Fprintln(os.Stderr, "       or retry with: wb2api autostart on --startup-folder")
			return 1
		}
		fmt.Printf("[OK] autostart enabled (%s)\n", info.Method)
		if info.RunValue != "" {
			fmt.Printf("     run key: %s\n", info.RunValue)
		}
		if info.StartupSet {
			fmt.Printf("     startup: %s\n", info.StartupFile)
		}
		return 0
	case "off", "disable":
		if _, err := autostart.Disable(); err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] %v\n", err)
		}
		fmt.Println("[OK] autostart disabled")
		return 0
	case "status":
		info, err := autostart.Status()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
			return 1
		}
		if !info.Enabled {
			fmt.Println("autostart: off")
			return 0
		}
		fmt.Printf("autostart: on (%s)\n", info.Method)
		if info.RunValue != "" {
			fmt.Printf("  run key: %s\n", info.RunValue)
		}
		if info.StartupSet {
			fmt.Printf("  startup: %s\n", info.StartupFile)
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: wb2api autostart on|off|status [--startup-folder] [--dir <data-dir>]")
		return 2
	}
}

// findDefaultConfig 当用户未显式传入自定义 config 路径且当前工作目录无 config.json 时，
// 探测环境变量 WB2API_HOME 及系统标准数据目录（Windows: %USERPROFILE%\.workbuddy2api，
// Unix: ~/.workbuddy2api），返回 (绝对 config 路径, 所在目录)；若均不存在则返回空。
func findDefaultConfig() (string, string) {
	candidates := make([]string, 0, 3)
	if h := os.Getenv("WB2API_HOME"); h != "" {
		candidates = append(candidates, h)
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		candidates = append(candidates, filepath.Join(userProfile, ".workbuddy2api"))
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, filepath.Join(home, ".workbuddy2api"))
	}
	for _, dir := range candidates {
		dir = strings.Trim(strings.TrimSpace(dir), "\"'")
		if dir == "" {
			continue
		}
		cfg := filepath.Join(dir, "config.json")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			abs, d := resolveConfigPath(cfg)
			return abs, d
		}
	}
	return "", ""
}

// resolveConfigPath 把 -config 解析为绝对路径，并给出进程应切换的工作目录。
//
// 返回 (绝对 config 路径, 应 chdir 的目录)；cfgPath 为空时原样返回且不切换。
// 会先剔除首尾空白与包裹引号（防止命令行/脚本复杂传参残留外层双引号在 Windows 下导致非法路径报错）。
// 相对 config 路径（如默认的 "config.json"）会被规整为 "当前目录/xxx/config.json"，
// chdir 目标即其所在目录——此时与进程原 CWD 相同，等价于空操作，不改变既有语义。
func resolveConfigPath(cfgPath string) (string, string) {
	cleaned := strings.Trim(strings.TrimSpace(cfgPath), "\"'")
	if cleaned == "" {
		return cfgPath, ""
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned, ""
	}
	return abs, filepath.Dir(abs)
}

// ignoreWriteErr 把 writer 的写错误吞掉（始终报告"全部写入"）。
// 用途：日志镜像里控制台可能已失效（托盘 FreeConsole 后），不能让它的错误
// 短路 io.MultiWriter 的后续目标（否则面板日志环丢日志）。
type ignoreErrWriter struct{ w io.Writer }

func (i ignoreErrWriter) Write(p []byte) (int, error) {
	_, _ = i.w.Write(p)
	return len(p), nil
}

func ignoreWriteErr(w io.Writer) io.Writer { return ignoreErrWriter{w: w} }

// checkUpdateAndNotify 检查新版本并在有更新时弹出 Windows 通知（托盘气泡）。
// 失败一律静默（离线环境不应产生噪音），只写一行日志。
func checkUpdateAndNotify(current string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res := panel.CheckUpdateFor(ctx, current, true)
	if res.Error != "" {
		log.Printf("[update] 检查失败（忽略）: %v", res.Error)
		return
	}
	if !res.Available {
		log.Printf("[update] 已是最新（当前 %s，清单 %s）", current, res.Latest)
		return
	}
	log.Printf("[update] 发现新版本 %s（当前 %s）", res.Latest, current)
	_ = tray.Notify("发现新版本 "+res.Latest,
		"当前 "+current+" → 最新 "+res.Latest+"。打开控制台「检查更新」查看下载地址。")
}
