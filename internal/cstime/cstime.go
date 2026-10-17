// Package cstime 统一"上游自然日"口径：**固定 +08:00（CST）**，与账号池的日切
// （`pool.currentDayCST`）、旅行日（`scheduler.travelDay`）、跨日重置（`cmd/server`
// 的 00:00 CST 循环）一致。
//
// 为什么需要单独一个包（2026-10-03，第七轮审计 F1）：本仓已经有 4 处"今天/昨天"参与
// **上游语义**（每日活跃奖励去重、补签卡日期、漏签判定、用量日桶），但它们原先各写各的 ——
// 有的用固定 CST、有的用**宿主本地时区**。在未设 TZ 的 UTC 容器里，CST 00:00–08:00 这段
// 窗口会算出"差一天"的日期，后果是实打实的：
//   - 国际版每日活跃奖励被重复领取 / 被误判为已领；
//   - **补签卡被用在错误的日期**（消耗品！）；
//   - "昨日漏签"误判 → 面板/任务中心的提示与决策出错。
//
// 用固定偏移而非 time.LoadLocation("Asia/Shanghai")：容器常缺 tzdata，LoadLocation 会失败。
package cstime

import "time"

// Zone 上游自然日时区（中国标准时间，固定 +08:00）。
var Zone = time.FixedZone("CST", 8*60*60)

// dayLayout 自然日格式。
const dayLayout = "2006-01-02"

// Day 返回 t 所属的 CST 自然日（"2006-01-02"）。
func Day(t time.Time) string { return t.In(Zone).Format(dayLayout) }

// Today 返回当前 CST 自然日。
func Today() string { return Day(time.Now()) }

// Yesterday 返回"CST 自然日的昨天"：先换算到 CST 再减一天，避免在
// "本地时间与 CST 不同日"的窗口里算错（例如 UTC 17:00 = CST 次日 01:00）。
func Yesterday() string { return Day(time.Now().In(Zone).AddDate(0, 0, -1)) }
