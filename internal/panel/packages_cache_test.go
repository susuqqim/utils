package panel

import (
	"testing"
	"time"
)

// 【D1 回归，2026-10-21】积分构成页必须缓存。
//
// 该接口**按账号逐个打上游计费端点**（5 个号并发 3 → 实测稳定 ~1.0s）。
// 修复前每切一次页就打 5 次上游；积分包到期以天计，5 分钟缓存足够新。
func TestPackagesCacheTTL(t *testing.T) {
	p := &Panel{}
	now := time.Now()

	// 空缓存：未命中
	if _, ok := p.cachedPackages(now); ok {
		t.Fatal("初始状态不应命中缓存")
	}

	// 写入后立即读：命中且返回同一 payload
	payload := map[string]any{"accounts": []any{}, "cached": true}
	p.storePackages(payload, now)
	got, ok := p.cachedPackages(now.Add(time.Minute))
	if !ok {
		t.Fatal("1 分钟后应命中缓存")
	}
	if got["cached"] != true {
		t.Errorf("命中时应标记 cached=true，实得 %#v", got)
	}

	// 超过 TTL：不命中（必须重新打上游）
	if _, ok := p.cachedPackages(now.Add(packagesTTL + time.Second)); ok {
		t.Errorf("%v 后应过期", packagesTTL)
	}
}

// TTL 取值守卫：太长会让用户看不到新积分，太短等于没缓存。
func TestPackagesTTLReasonable(t *testing.T) {
	if packagesTTL < time.Minute {
		t.Errorf("packagesTTL=%v 太短，起不到缓存作用", packagesTTL)
	}
	if packagesTTL > 30*time.Minute {
		t.Errorf("packagesTTL=%v 太长，积分变化会长时间看不到", packagesTTL)
	}
}
