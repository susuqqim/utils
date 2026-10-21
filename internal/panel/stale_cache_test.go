package panel

import (
	"testing"
	"time"
)

// 【stale-while-revalidate 回归，2026-10-21 生产事故】
//
// 事故现象：只有 TTL 判定时，缓存一过期就必须同步打上游；上游 TLS 握手抖动
// （实测 `net/http: TLS handshake timeout`）时，/panel/api/models 卡死 30s+ 客户端超时，
// 而同机的 overview/packages 仍是毫秒级。只读展示页不该被上游抖动拖死。
func TestStaleModelsSurvivesExpiry(t *testing.T) {
	p := &Panel{}
	now := time.Now()

	// 空缓存：既不该命中新鲜缓存，也不该有 stale 可用
	if _, ok := p.cachedModels("all", now); ok {
		t.Fatal("初始状态不应命中新鲜缓存")
	}
	if _, ok := p.staleModels("all"); ok {
		t.Fatal("初始状态不应有 stale 数据")
	}

	// 写入后过期：新鲜缓存失效，但 stale 必须仍在（这是不卡死的前提）
	entries := []map[string]any{{"id": "cn:glm-5.2"}}
	p.storeModels("all", entries, nil, now)
	expired := now.Add(catalogTTL + time.Second)

	if _, ok := p.cachedModels("all", expired); ok {
		t.Fatal("超 TTL 后不应命中新鲜缓存")
	}
	e, ok := p.staleModels("all")
	if !ok {
		t.Fatal("超 TTL 后必须仍有 stale 数据可供立即返回")
	}
	if len(e.entries) != 1 || e.entries[0]["id"] != "cn:glm-5.2" {
		t.Errorf("stale 数据内容不对: %#v", e.entries)
	}
}

// 后台刷新必须去重：同一时刻只允许一个在跑，否则多个面板请求会同时打上游。
func TestBeginRefreshIsExclusive(t *testing.T) {
	p := &Panel{}
	if !p.beginRefresh("models") {
		t.Fatal("首次应取得刷新权")
	}
	if p.beginRefresh("models") {
		t.Error("重复获取必须失败（已有刷新在途）")
	}
	if !p.beginRefresh("packages") {
		t.Error("不同 key 之间不应互相阻塞")
	}
	p.endRefresh("models")
	if !p.beginRefresh("models") {
		t.Error("释放后应能再次取得")
	}
	p.endRefresh("models")
	p.endRefresh("packages")
}

// stalePackages 语义同 staleModels。
func TestStalePackagesSurvivesExpiry(t *testing.T) {
	p := &Panel{}
	now := time.Now()
	if _, ok := p.stalePackages(); ok {
		t.Fatal("初始状态不应有 stale packages")
	}
	p.storePackages(map[string]any{"accounts": []any{}, "cached": true}, now)
	if _, ok := p.cachedPackages(now.Add(packagesTTL + time.Second)); ok {
		t.Fatal("超 TTL 后不应命中新鲜缓存")
	}
	if _, ok := p.stalePackages(); !ok {
		t.Fatal("超 TTL 后必须仍有 stale packages")
	}
}
