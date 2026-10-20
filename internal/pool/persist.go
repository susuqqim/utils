// 持久化：本地 state.json 落盘/加载、Redis 快照镜像（StoreSnapshotter）、
// 后台 flusher、择新恢复（RestoreFromSnapshot）。
package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"workbuddy2api/internal/gosafe"

	"workbuddy2api/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 100

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
// 与本地 state.json 并存，作启动恢复备份：快照比本地新才采用，否则本地优先。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("[pool] 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("[pool] 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr == nil && !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.mu.Lock()
		p.applySnapshotLocked(snap)
		p.mu.Unlock()
		p.dirty.Store(true)
		log.Printf("[pool] 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	log.Printf("[pool] 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// startFlusher 每 flushInterval 检查 dirty 标志，有变更则 saveLocked 落盘。
func (p *Pool) startFlusher() {
	interval := flushInterval // 在启动 goroutine 前同步读取，避免与测试对 flushInterval 的恢复写竞争
	go gosafe.Do("pool flusher", func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			// 迭代级兜底：顶层 recover 会让整个 flusher 协程结束 → **落盘永久停摆**且无迹象。
			// defer 解锁：panic 时 Go 的 mutex 不会自动释放，否则 recover 后全池死锁。
			gosafe.Loop("pool flusher", func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.flushLocked()
			})
		}
	})
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.mu.Lock()
	p.flushLocked()
	p.mu.Unlock()
}

// flushLocked 落盘一次：有变更才写；写失败时**回置 dirty**，让下一轮重试，
// 而不是把这一轮的全部变更（积分 / 冷却 / 计数 / 用量）静默丢掉。
// 调用方必须持有 p.mu（saveLocked 的前置条件）。
func (p *Pool) flushLocked() {
	if !p.dirty.Swap(false) {
		return
	}
	if !p.saveLocked() {
		p.dirty.Store(true)
	}
}

// load 从 state.json 恢复池状态；文件不存在 / 不可读 / 解析失败时按"空池"启动（静默跳过）。
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a:                &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:          s.Credits,
			creditsTotal:     s.CreditsTotal,
			creditsUsed:      s.CreditsUsed,
			dailyCreditsUsed: s.DailyCreditsUsed,
			dailyTokensUsed:  s.DailyTokensUsed,
			dailyDate:        s.DailyDate,
			totalTokens:      s.TotalTokens,
			earliestExpire:   s.EarliestExpire,
			urgent24hRemain:  s.Urgent24hRemain,
			urgent72hRemain:  s.Urgent72hRemain,
			packages:         s.Packages,
			lastResourceSync: s.LastResourceSync,
			disabled:         s.Disabled,
			restrictedUntil:  s.RestrictedUntil,
			paused:           s.Paused,
			reason:           s.Reason,
			until:            s.Until,
			coolKind:         s.CoolKind,
			successCount:     s.SuccessCount,
			errTotal:         errTotal,
			lastErr:          s.LastErr,
			lastSuccess:      s.LastSuccess,
			tokenUsage:       s.TokenUsage,
			softStreak:       s.SoftStreak,
			dailyMarks:       s.DailyMarks,
		}
		if len(e.packages) > 0 {
			p.recomputeFromPackagesLocked(e, time.Now())
		}
		p.byUID[uid] = e
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}

// saveLocked 把内存状态原子落盘；返回是否真的写成（false = 失败，调用方应回置 dirty 重试）。
// stateFp 为空表示未配置持久化，视作"无需重试的成功"。
func (p *Pool) saveLocked() bool {
	if p.stateFp == "" {
		return true
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return false
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return false
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		// 回退直接覆盖写（单文件挂载、跨设备或 Windows 句柄占用）
		if werr := os.WriteFile(p.stateFp, raw, 0o600); werr != nil {
			p.notePersistFail(fmt.Errorf("rename %w, write fallback %w", err, werr))
			return false
		}
		_ = os.Remove(tmp)
	}

	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("[pool] state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}
	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
	return true
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打一条详细 WARN：包含路径、err、目录权限/属主、当前 uid/gid、
// 修复建议（chown 或改用 named volume）；随后每 persistLogEvery 次再复报一条，避免刷屏。
// 恢复成功的日志由 saveLocked 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("WARN: [pool] state.json 落盘失败（首次详报）: path=%s err=%v %s",
			p.stateFp, err, persistFailDiag(p.stateFp))
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("ERR: [pool] state.json 连续落盘失败 %d 次: path=%s err=%v",
			p.persistFails, p.stateFp, err)
	}
	p.persistFails++
}

// persistFailDiag 收集落盘失败时的环境诊断信息：目录是否存在、权限/属主、
// 当前进程 uid/gid，并给出对齐修复建议。供首错详报，定位 issue #52 这类
// bind mount 目录属主不匹配导致不可写的场景。
func persistFailDiag(stateFp string) string {
	dir := filepath.Dir(stateFp)
	var b strings.Builder
	info, statErr := os.Stat(dir)
	switch {
	case statErr != nil:
		fmt.Fprintf(&b, "stat_dir=%s err=%v（目录不存在或不可访问）", dir, statErr)
	case info == nil:
		fmt.Fprintf(&b, "stat_dir=%s info=nil", dir)
	default:
		mode := info.Mode().Perm()
		fmt.Fprintf(&b, "dir=%s mode=%o owner_uid=%d owner_gid=%d",
			dir, mode, statUID(info), statGID(info))
	}
	if uid, gid := os.Getuid(), os.Getgid(); uid >= 0 && gid >= 0 {
		fmt.Fprintf(&b, " proc_uid=%d proc_gid=%d", uid, gid)
	}
	b.WriteString(" fix=chown -R 10001:10001 ./data 或改用 named volume（见 docker-compose.yml）")
	return b.String()
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		var pkgs []PackageItem
		if len(e.packages) > 0 {
			pkgs = make([]PackageItem, len(e.packages))
			copy(pkgs, e.packages)
		}
		sf.Accounts[uid] = stateAccount{
			Credits:          e.credits,
			CreditsTotal:     e.creditsTotal,
			CreditsUsed:      e.creditsUsed,
			DailyCreditsUsed: e.dailyCreditsUsed,
			DailyTokensUsed:  e.dailyTokensUsed,
			DailyDate:        e.dailyDate,
			TotalTokens:      e.totalTokens,
			TokenUsage:       e.tokenUsage,
			EarliestExpire:   e.earliestExpire,
			Urgent24hRemain:  e.urgent24hRemain,
			Urgent72hRemain:  e.urgent72hRemain,
			Packages:         pkgs,
			LastResourceSync: e.lastResourceSync,
			Disabled:         e.disabled,
			RestrictedUntil:  e.restrictedUntil,
			Paused:           e.paused,
			Reason:           e.reason,
			Until:            e.until,
			CoolKind:         e.coolKind,
			SuccessCount:     e.successCount,
			ErrTotal:         e.errTotal,
			LastSuccess:      e.lastSuccess,
			LastErr:          e.lastErr,
			SoftStreak:       e.softStreak,
			DailyMarks:       copyDailyMarks(e.dailyMarks),
		}
	}
	return sf
}

// copyDailyMarks 复制每日标记（落盘用；nil 安全 —— 避免把共享 map 交给序列化后的并发写）。
func copyDailyMarks(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
