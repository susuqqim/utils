// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 六类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"workbuddy2api/internal/cstime"
	"workbuddy2api/internal/gosafe"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即六类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：<=0 时禁用分桶。
	ExpiringSoonWindow time.Duration

	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool
	// IncludeDisabledInTasks 保号类四任务是否覆盖已禁用账号（schedule.include_disabled_in_tasks）。
	// 缺省 false = 禁用号仍跳过（零回归）。
	IncludeDisabledInTasks bool
	// EnterpriseGate 企业号能力门控（S5 第 2 步，上游 7e3bc51）。**默认 false**：
	// 开启后企业版账号（auth 带非空 enterpriseId）跳过个人成长体系类任务 —— 企业号没有个人成长
	// 体系，上游对其返回 400/10001，此前网关每天空发注定失败的请求（浪费调用 + 污染失败率）。
	// 判定一律走 pool.Status.IsEnterprise()（只认非空字符串；空白/缺失判个人号）。
	EnterpriseGate bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu            sync.Mutex
	adoptTried    map[string]string
	lastDailyChat map[string]string // 国际版每日活跃打卡记录：uid → 自然日（CST，内存镜像）
	dailyChatMu   sync.Mutex        // 国际版每日活跃打卡串行化（与 checkinMu 同口径）
	streakMu      sync.Mutex        // 连登兑换/补签串行化（消耗品，必须防并发）
	retryMu       sync.Mutex        // 保护 streakCatchUpArmed（与 streakMu 分离，避免锁序纠缠）
	// streakCatchUpArmed 是否已有补跑者在排队（同一时刻只允许一个）。
	streakCatchUpArmed bool
	// penaltyUntil 账号级"别再打"惩罚期（429/11140，见 notePenalty）：进程内，不落盘。
	penaltyUntil map[string]time.Time

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	balanceInterval atomic.Int64
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.GrowthHours) == 0 {
		cfg.GrowthHours = []int{1}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	if len(cfg.BlackcatHours) == 0 {
		if len(cfg.CatHours) > 0 {
			cfg.BlackcatHours = cfg.CatHours
		} else {
			cfg.BlackcatHours = []int{23}
		}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	if cfg.CatDisabled && !cfg.BlackcatDisabled {
		cfg.BlackcatDisabled = true
	}
	if cfg.BlackcatDisabled && !cfg.CatDisabled {
		cfg.CatDisabled = true
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		lastDailyChat: make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// includeDisabledInTasks 读取"保号任务覆盖禁用号"开关（请求路径每次取一次快照）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新该开关（面板保存配置后调用）。用**独立 setter** 而不是
// 并入 Reconfigure：后者已是 14 个位置参数，继续加会显著提高漏传/错位风险。
// enterpriseGate 读取"企业号跳过个人成长体系"开关（S5 第 2 步）。默认 false ⇒ 零行为变更。
func (s *Scheduler) enterpriseGate() bool {
	if s == nil {
		return false
	}
	return s.cfg.EnterpriseGate
}

// SetEnterpriseGate 热更新该开关（面板保存配置后调用，与 IncludeDisabledInTasks 同一体例）。
// EnterpriseGate 导出访问器（面板侧 taskcenter 要跨包读取该开关）。
// 语义与 SetEnterpriseGate 成对；nil 接收者安全返回 false。
func (s *Scheduler) EnterpriseGate() bool { return s.enterpriseGate() }

func (s *Scheduler) SetEnterpriseGate(v bool) {
	if s == nil {
		return
	}
	s.cfg.EnterpriseGate = v
}

func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
//
// catHours/catDisabled 与 blackcatHours/blackcatDisabled 是**两个独立键**（互为别名式互通，
// 见 config.Schedule 与 New 的归一），必须各自写各自的字段：旧实现对两者都写 Cat* 与
// Blackcat*，于是面板保存配置会把 `cat_hours`/`cat_enabled` 静默改写成 `blackcat_*` 的值
// （启动时若两键不同，热重载后夜猫子时点会无声漂移）。
func (s *Scheduler) Reconfigure(
	checkinHours, travelHours, activityHours, keepaliveHours, catHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, catDisabled, blackcatDisabled, growthDisabled bool,
) {
	// 与 New 同口径的"或"归一：夜猫子任一开关为关即整体关闭（isCatDisabled/dispatch 也按或判定）。
	if catDisabled && !blackcatDisabled {
		blackcatDisabled = true
	}
	if blackcatDisabled && !catDisabled {
		catDisabled = true
	}
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(catHours) > 0 {
		s.cfg.CatHours = catHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.CatDisabled = catDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号。
func poke(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为 **CST（中国标准时间）小时**（0-23）。
//
// 口径（第十轮审计 F4）：本仓所有日界（签到日、补签日、活跃日、用量日桶）都是固定 CST，
// 项目自带的 docker-compose 也设 TZ=Asia/Shanghai —— 若槽位按宿主本地时区解释，
// 非 CST 宿主上会出现"早上 9 点失败后、晚上 21 点已跨 CST 日 → 当天不再重试"这类错位。
// 返回值是绝对时刻（带 CST 位置），比较/timer 语义不受影响。
func nextFire(now time.Time, hours []int) time.Time {
	now = now.In(cstime.Zone)
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, cstime.Zone)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskCat
	taskGrowth
)

func (s *Scheduler) isCheckinDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.CheckinDisabled
}

func (s *Scheduler) isTravelDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.TravelDisabled
}

func (s *Scheduler) isActivityDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ActivityDisabled
}

func (s *Scheduler) isKeepaliveDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.KeepaliveDisabled
}

func (s *Scheduler) isGrowthDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.GrowthDisabled
}

func (s *Scheduler) isCatDisabled() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.CatDisabled || s.cfg.BlackcatDisabled
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if !s.cfg.CatDisabled && !s.cfg.BlackcatDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CatHours), taskCat})
	}
	if !s.cfg.GrowthDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.GrowthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		stop := false
		// 每轮独立兜底：若让 panic 冒到调用方的顶层 recover，Run 协程会结束 →
		// 签到/旅行/活跃/保活/夜猫**从此再不执行**，而网关照常服务、日志零报错，
		// 只能靠"今天没签到"这种几天后的现象发现（比崩溃更隐蔽）。
		if gosafe.Loop("scheduler tick", func() { stop = s.runOnce(ctx) }) {
			// 本轮 panic：加 1s 退避，避免"每轮都 panic"变成 100% CPU 紧循环。
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		if stop {
			return
		}
	}
}

// runOnce 执行一轮"等到下一个排程点 → 派发到点任务"；返回 true 表示收到退出信号。
func (s *Scheduler) runOnce(ctx context.Context) bool {
	next, kinds := s.nextWake(time.Now())
	if next.IsZero() {
		// 六类任务全部禁用：不空转，只等退出信号或排程变更重算。
		select {
		case <-ctx.Done():
			return true
		case <-s.rearmSchedule:
			return false
		}
	}
	switch s.waitSlot(ctx, next, wallclockCheckStep) {
	case slotCancel:
		return true
	case slotRearm:
		return false
	case slotFired:
		// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
		for _, k := range kinds {
			// dispatch 在本协程内**内联**跑签到/旅行/活跃/猫猫/余额同步（含上游 JSON 解析），
			// 故再单独兜一层：一轮里某个任务炸了不该连累同轮其它任务。
			kind := k
			gosafe.Loop("scheduler dispatch", func() { s.dispatch(kind) })
		}
	}
	return false
}

// dispatch 按任务类型分发到对应执行函数。脚本类失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
func (s *Scheduler) dispatch(k taskKind) {
	switch k {
	case taskCheckin:
		s.RunCheckinNow()
	case taskTravel:
		s.RunTravelNow()
	case taskActivity:
		s.RunActivityNow()
	case taskKeepalive:
		s.RunKeepaliveNow()
	case taskCat:
		s.RunBlackcatNow()
	case taskGrowth:
		// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
		s.schedMu.Lock()
		hook := s.cfg.GrowthHook
		s.schedMu.Unlock()
		if hook != nil {
			hook()
		}
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
// force 可选：为 true 时强制执行（如面板手动触发），忽略 CheckinDisabled 开关。
// 连带任务（streak / daily chat）各自遵守各自的开关约束：
// - streak 属于签到衍生，跟从 checkin
// - global daily chat 属于国际版每日打卡
func (s *Scheduler) RunCheckinNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isCheckinDisabled() {
		return
	}
	if _, err := s.CheckinAll(); err != nil {
		// 撞车（ErrBusy）时只记日志、**仍然跑连带任务**（第十一轮审计 F3）：
		// 若这里 return，则 9 点与 21 点两次都撞车（或手动点击恰好撞上定时）会让当天的
		// 连登兑换/补签/每日活跃**一次都不跑** —— 那是纯损失；并发风险已由
		// RunStreakBonusNow/RunDailyChatGlobalNow 各自的 TryLock 独立兜住。
		log.Printf("scheduled checkin skipped（连带任务照常执行）: %v", err)
	}
	s.RunStreakBonusNow(isForce)
	s.RunDailyChatGlobalNow(isForce)
}

// RunDailyChatGlobalNow 国际版每日活跃对话打卡：每日首次活跃赠送 30~50 积分奖励。
//
// 幂等与并发（第十轮审计 F9/F2）：进程内 tryLock 防并发，去重标记走**持久化**的
// `Pool.DailyMarkDoneToday(uid, "daily_chat")`（内存版重启即失效，会在同一天再打一次）。
func (s *Scheduler) RunDailyChatGlobalNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isCheckinDisabled() {
		return
	}
	if !s.dailyChatMu.TryLock() {
		log.Printf("global daily chat: 已有一次在跑，跳过本次")
		return
	}
	defer s.dailyChatMu.Unlock()
	statuses := s.cfg.Pool.List()
	today := cstime.Today() // 上游自然日（CST）：本地时区在 UTC 宿主上会差一天 → 奖励被重复领取
	for _, st := range statuses {
		// ①B：每日真实对话打卡属"真实对话类" ⇒ 禁用号与**暂停号**都跳过。
		if st.Disabled || st.Paused || (s.enterpriseGate() && st.IsEnterprise()) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.IsGlobal() {
			continue
		}
		s.mu.Lock()
		alreadyActive := s.lastDailyChat[st.UID] == today
		s.mu.Unlock()
		if !isForce {
			if alreadyActive || s.cfg.Pool.DailyMarkDoneToday(st.UID, "daily_chat") {
				continue
			}
		}
		// 按需刷新 token
		if a.NeedsRefresh(checkinRefreshSkew) {
			_ = s.cfg.Upstream.RefreshToken(a)
		}
		if err := s.cfg.Upstream.DailyChatGlobal(a); err != nil {
			log.Printf("global daily chat %s: %v", logfmt.UID8(st.UID), err)
		} else {
			s.mu.Lock()
			s.lastDailyChat[st.UID] = today
			s.mu.Unlock()
			s.cfg.Pool.NoteDailyMark(st.UID, "daily_chat", today) // 持久化：重启当天不再重打
			log.Printf("global daily chat %s: 每日活跃打卡完成（获取每日 30~50 积分奖励）", logfmt.UID8(st.UID))
		}
		remain, total, err := s.cfg.Upstream.UserResource(a)
		if err == nil {
			s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		}
	}
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用号是否跳过取决于
// schedule.include_disabled_in_tasks（缺省跳过）。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled && !s.includeDisabledInTasks() {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.UID8(st.UID), err)
				}
			}
		}

		// 国内版（CN Realm）：标准计费中心每日签到
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.UID8(st.UID), err)
				// 限流/风控时**不再追加余额查询**：同一账号同一 tick 内少打一次，
				// 并记惩罚期让后续排程动作也停手（第十轮审计 F3）。
				if d, kind := upstreamAccountPenalty(err); d > 0 {
					s.notePenalty(st.UID, err)
					oc.Detail = joinDetail(oc.Detail, "已暂停排程："+kind)
					failN++
					out = append(out, oc)
					continue
				}
			}
		} else {
			oc.Status = CheckinOK
		}
		remain, total, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.UID8(st.UID), err)
			// **不得用"余额查询失败"覆盖签到结论**（第十轮审计 F7）：签到若已成功/已签，
			// 把 status 改成 fail 会让面板"今日签到"徽标误显示失败、failN 虚高，
			// 而且 NoteCheckin 会把当天状态写成 fail。余额错误只进 detail。
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			switch oc.Status {
			case CheckinOK:
				okN++
			case CheckinAlready:
				alreadyN++
			default:
				failN++
			}
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	// 记录每个账号的签到结果（运行态），供面板卡片展示"今日是否签到"。
	for i := range out {
		s.cfg.Pool.NoteCheckin(out[i].UID, string(out[i].Status))
	}
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// force 可选：为 true 时强制执行，忽略 ActivityDisabled 开关。
// 禁用账号跳过（受 schedule.include_disabled_in_tasks 控制）；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
func (s *Scheduler) RunActivityNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isActivityDisabled() {
		return
	}
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !s.includeDisabledInTasks() {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.UID8(a.UID), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				time.Sleep(activityReportGap) // 账号内 5 条之间间隔，避免秒发风控
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// force 可选：为 true 时强制执行，忽略 KeepaliveDisabled 开关。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isKeepaliveDisabled() {
		return
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !s.includeDisabledInTasks() {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if s.penalized(st.UID) { // 限流/风控期内不刷 token（刷新失败会累计 session dead 误杀号）
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.UID8(st.UID), err)
			var ue *upstream.Error
			// 覆盖开关打开后禁用号也会跑保活：此时 12153 不再累计连败/不再打"连续 N 次—禁用"
			// 误导日志 —— NoteSessionDead 会**无条件**覆盖 e.reason="12153 session dead"，
			// 把该号原本的禁用原因（运维手工禁用 / 11140 风控等）抹掉（审查 D 的 P1-2）。
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.UID8(st.UID), err)
		}
	}
}

// RunBalanceRefreshNow 并发查询余额并更新池内 credits（默认跳过禁用账号；
// schedule.include_disabled_in_tasks=true 时含禁用号）。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
func (s *Scheduler) RunBalanceRefreshNow() {
	// 并发上限（第十轮审计 F6）：旧实现对每个账号起一个 goroutine，账号数十/上百时
	// 瞬时并发＝账号数，且与每小时资产同步、到点排程任务三路同时压同一批账号。
	// 8 是"够快但不像攻击"的折中（上游是同一批账号，过并发只会触发限流/风控）。
	const balanceRefreshConcurrency = 8
	sem := make(chan struct{}, balanceRefreshConcurrency)
	var wg sync.WaitGroup
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !s.includeDisabledInTasks() {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if s.penalized(st.UID) { // 限流/风控惩罚期内不再打（每分钟级循环最容易"越试越紧"）
			continue
		}
		wg.Add(1)
		sem <- struct{}{} // 并发上限：满员时此处阻塞（不丢账号，只是排队）
		// uid 显式取值进闭包（原实现是 goroutine 形参；闭包捕获外层变量最容易踩到
		// "共享变量"陷阱，显式复制最稳妥）。
		uid := st.UID
		go gosafe.Do("scheduler balance detail", func() {
			defer wg.Done()
			defer func() { <-sem }() // panic 也要归还信号量（gosafe 兜住 panic 后仍会执行 defer）
			detail, err := s.cfg.Upstream.ResourceDetail(a)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				s.notePenalty(uid, err)
				return
			}
			var pkgs []pool.PackageItem
			for _, pkg := range detail.PackageList {
				pkgs = append(pkgs, pool.PackageItem{
					Remain: pkg.Remain,
					Expire: pkg.Expire,
				})
			}
			s.cfg.Pool.UpdateResourceDetail(uid, detail.Remain, detail.EarliestExpire, detail.Urgent24hRemain, detail.Urgent72hRemain, pkgs...)
			s.cfg.Pool.ReenableIfCredits(uid, detail.Remain, detail.Size)
		})
	}
	wg.Wait()
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go gosafe.Do("scheduler tick", func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop()
			case <-timer.C:
				// 迭代级兜底：顶层 recover 会让余额刷新循环结束 → 资产画像永久停止更新
				// （面板/调度仍显示陈旧积分，且无任何报错）。
				gosafe.Loop("balance refresh", func() { s.RunBalanceRefreshNow() })
			}
		}
	})
}

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

// TryCheckinLock / UnlockCheckin 暴露"签到中"这把锁给面板：单号签到（HTTP 协程里直接打
// DailyCheckin）必须与定时 CheckinAll 互斥 —— 否则同一账号被并发签到，且后到的结果会
// 覆盖当天状态（第十轮审计 F5）。TryLock 语义：已有人在跑时立即返回 false（不排队）。
func (s *Scheduler) TryCheckinLock() bool { return s.checkinMu.TryLock() }

// UnlockCheckin 释放 TryCheckinLock 取得的锁。
func (s *Scheduler) UnlockCheckin() { s.checkinMu.Unlock() }

// ---------------------------------------------------------------------------
// 上游"别再打这个账号"的信号（第十轮审计 F3）
//
// 排程此前完全不看 429/11140：余额刷新每 5 分钟、资产同步每小时都会继续打同一个
// 被限流或账号级风控的账号 —— 正是 AGENTS.md 自己写的"越试越紧"。这里做**进程内**
// 惩罚期（不落盘：惩罚是"当下别打"，重启后重新试一次即可，落盘反而会让一次误判
// 长期生效）。
// ---------------------------------------------------------------------------

// upstreamAccountPenalty 把上游错误翻译成"该账号应暂停多久"。
func upstreamAccountPenalty(err error) (time.Duration, string) {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return 0, ""
	}
	switch ue.Kind {
	case upstream.ErrSoftRate:
		return 30 * time.Minute, "429 限流"
	case upstream.ErrAccountFault:
		return 6 * time.Hour, "账号级风控/授权（11140 等）"
	}
	return 0, ""
}

// notePenalty 记录账号惩罚期（同一账号重复命中会刷新截止时间）。
func (s *Scheduler) notePenalty(uid string, err error) {
	d, kind := upstreamAccountPenalty(err)
	if d <= 0 {
		return
	}
	until := time.Now().Add(d)
	s.mu.Lock()
	if s.penaltyUntil == nil {
		s.penaltyUntil = map[string]time.Time{}
	}
	first := s.penaltyUntil[uid].Before(time.Now())
	s.penaltyUntil[uid] = until
	s.mu.Unlock()
	if first {
		log.Printf("WARN: 排程暂停账号 %s：%s → 至 %s（期间不再对它发任何排程请求，避免越试越紧）",
			logfmt.UID8(uid), kind, until.Format("15:04"))
	}
}

// penalized 报告账号是否处于惩罚期内（应跳过本次排程动作）。
func (s *Scheduler) penalized(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.penaltyUntil[uid]
	return ok && time.Now().Before(until)
}
