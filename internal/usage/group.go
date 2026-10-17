// group.go —— S4 第①步：用量分组的**键构造**（纯函数，不改动既有 Recorder 行为）。
//
// 为什么单独抽出来：分组键的正确性有两条"隐蔽错账"陷阱，必须能被单测直接锁死 ——
//  1. **realm 必带**：本地是双域网关（cn= copilot.tencent.com / global= www.workbuddy.ai），
//     两域存在**同名模型**。若只按 bareModel 聚合，会把两域用量合并成一个数字：
//     表面看是"用量偏低/偏高"，实则账目错配，且不会报任何错误。
//  2. **按天必须用 CST 自然日**：上游的"每日"语义是 CST（UTC+8）。宿主跑在 UTC 时区时，
//     用本地日历日切分会把 08:00 CST 之前的用量算进前一天（差一天，跨日看报表就会对不上）。
package usage

import "time"

// GroupKey 一个用量聚合维度。零字段表示"该维度不参与分组"。
type GroupKey struct {
	Realm string `json:"realm,omitempty"` // cn | global（**按模型聚合时必须带**）
	Model string `json:"model,omitempty"` // bareModel（不含 realm 前缀）
	UID   string `json:"uid,omitempty"`
	Day   string `json:"day,omitempty"` // CST 自然日 YYYY-MM-DD
}

// ModelKey 按**域 + 模型**分组（S4：跨域同名模型必须分成两行）。
func ModelKey(realm, model string) GroupKey {
	return GroupKey{Realm: realm, Model: model}
}

// AccountKey 按账号分组。
func AccountKey(uid string) GroupKey {
	return GroupKey{UID: uid}
}

// CSTDay 返回 ts 对应的上游自然日（CST, UTC+8）字符串 YYYY-MM-DD。
// 复用本包既有的 cstZone（与内部 cstime 包同口径）。
func CSTDay(ts time.Time) string {
	return ts.In(cstZone).Format("2006-01-02")
}

// DayKey 按 CST 自然日分组。
func DayKey(ts time.Time) GroupKey {
	return GroupKey{Day: CSTDay(ts)}
}

// DayKeyFor 按"某日 + 域 + 模型"分组（面板三视图组合用）。
func DayKeyFor(ts time.Time, realm, model string) GroupKey {
	return GroupKey{Day: CSTDay(ts), Realm: realm, Model: model}
}

// String 稳定可排序的键表示（同键必同串，便于做 map 键与日志）。
func (k GroupKey) String() string {
	s := ""
	if k.Day != "" {
		s += k.Day + "|"
	}
	if k.Realm != "" {
		s += k.Realm + "|"
	}
	if k.Model != "" {
		s += k.Model + "|"
	}
	if k.UID != "" {
		s += k.UID + "|"
	}
	return s
}

// BucketKey 用量桶的 **canonical 身份串**（S4 方案 A：全仓唯一的键生成入口）。
//
// 关于持久化兼容性：桶在文件里是以 `bucket{Scope,Realm,UID,Model}` **结构体**形式落盘的
// （见 usage.go 的 file/load），map 的字符串键**不被解析、不落盘、不参与往返**，因此把键生成
// 收敛到本函数是**纯身份层重构**，不会影响既有归档的读取（这也回答了"双份真相如何处理才安全"）。
// 格式保持与既有实现逐字一致（scope|realm|uid|model），便于日志/快照肉眼比对。
func BucketKey(scope, realm, uid, model string) string {
	return scope + "|" + realm + "|" + uid + "|" + model
}
