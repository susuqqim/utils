// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// force 可选：为 true 时强制执行，忽略 CatDisabled / BlackcatDisabled 开关。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow(force ...bool) {
	isForce := len(force) > 0 && force[0]
	if !isForce && s.isCatDisabled() {
		return
	}
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		// 夜猫子是任务体系中**唯一**整任务都是真实模型对话的（RunNightChats 逐条
		// ChatStream 发 glm-5.2），与"让位防风控"正面冲突 → 暂停号必须跳过它
		// （上游 b1a2284 的最终口径：旅行/成长/任务中心照常，只跳夜猫子）。
		if st.Disabled || st.Paused {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无 CN 任务体系，不发起任何上游调用
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
			continue
		}
		log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
		time.Sleep(activityAccountDelay)
	}
}
