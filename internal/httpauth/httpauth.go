// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// VerifyBearerTransition 在**主密钥**之外再接受一个**过渡密钥**（密钥轮换期使用）。
// 返回 (是否通过, 是否用了过渡密钥)。
//
// 为什么需要（2026-10-05 密钥轮换）：若轮换时只认新钥，所有"已经启动、把旧钥缓存在
// 内存里"的客户端会立刻 401 —— 包括正在跑的会话宿主（实测有 4 类活跃客户端持有旧钥：
// 本机代理、OpenCode、DSH 凭据、面板 localStorage）。过渡钥把这些客户端平滑带过去，
// 且**用旧钥的请求会打节流 WARN**，便于确认"还有谁没迁移"。
// 客户端全部迁移后把 config 的 api_key_previous 清空，旧钥立刻失效（轮换才算完成）。
func VerifyBearerTransition(r *http.Request, primary, previous string) (ok bool, usedPrevious bool) {
	if VerifyBearer(r, primary) {
		return true, false
	}
	if previous != "" && previous != primary && VerifyBearer(r, previous) {
		return true, true
	}
	return false, false
}

// prevKeyWarnNano 过渡钥使用告警的节流时间戳（每 scope 一条/5 分钟）。
var prevKeyWarnNano atomic.Int64

// WarnPreviousKeyUse 记录"本次请求用的是过渡密钥"，同一 scope 5 分钟内只打一条。
func WarnPreviousKeyUse(scope string) {
	const every = int64(5 * time.Minute)
	now := time.Now().UnixNano()
	if last := prevKeyWarnNano.Load(); now-last < every {
		return
	}
	prevKeyWarnNano.Store(now)
	log.Printf("WARN: [%s] 请求使用了过渡密钥 api_key_previous —— 该客户端**尚未**更新到新密钥"+
		"（轮换未完成；全部迁移后清空 api_key_previous 即可让旧钥失效）", scope)
}
