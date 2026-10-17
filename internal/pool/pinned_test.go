package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

func TestPinnedAccountRouting(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "cn1", Nickname: "账号1", AccessToken: "at1"}
	a2 := &auth.Auth{UID: "cn2", Nickname: "账号2", AccessToken: "at2"}
	a3 := &auth.Auth{UID: "cn3", Nickname: "账号3", AccessToken: "at3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)

	p.SetCredits("cn1", 100)
	p.SetCredits("cn2", 500)
	p.SetCredits("cn3", 1000)

	// 1. 未指定强制路由时，调度应遵循常规加权/分块
	got := p.Pick()
	if got == nil {
		t.Fatal("expected non-nil account")
	}

	// 2. 强制指定 cn1（即便其积分最低）
	realm, err := p.PinAccount("cn1")
	if err != nil {
		t.Fatalf("pin error: %v", err)
	}
	if realm != "cn" {
		t.Fatalf("expected realm cn, got %s", realm)
	}

	if p.PinnedUIDForRealm("cn") != "cn1" {
		t.Fatalf("expected pinned cn1, got %s", p.PinnedUIDForRealm("cn"))
	}

	// 连续 20 次选号必须 100% 命中 cn1
	for i := 0; i < 20; i++ {
		picked := p.Pick()
		if picked == nil || picked.UID != "cn1" {
			t.Fatalf("run %d: expected cn1, got %v", i, picked)
		}
	}

	// 3. 验证 Status 里的 Pinned 标识
	list := p.List()
	foundPinned := false
	for _, s := range list {
		if s.UID == "cn1" {
			if !s.Pinned {
				t.Fatalf("cn1 status should be pinned")
			}
			foundPinned = true
		} else {
			if s.Pinned {
				t.Fatalf("%s status should NOT be pinned", s.UID)
			}
		}
	}
	if !foundPinned {
		t.Fatal("cn1 not in list")
	}

	// 4. 重试轮次降级：当 cn1 已在 tried 排除列表中时，自动回退到其他可用账号
	tried := map[string]bool{"cn1": true}
	fallback := p.PickExcluding(tried)
	if fallback == nil || fallback.UID == "cn1" {
		t.Fatalf("expected fallback to other accounts, got %v", fallback)
	}

	// 5. 取消强制指定
	if err := p.UnpinAccount("cn1"); err != nil {
		t.Fatalf("unpin error: %v", err)
	}
	if p.PinnedUIDForRealm("cn") != "" {
		t.Fatalf("expected empty pinned UID after unpin")
	}

	// 6. 重启后重置语义：新创建的 Pool 不继承强制状态
	pNew := New("")
	pNew.Add(a1)
	pNew.Add(a2)
	if len(pNew.PinnedUIDs()) != 0 {
		t.Fatalf("expected new pool to have empty pinnedUIDs, got %v", pNew.PinnedUIDs())
	}
}

func TestPinnedAccountMultiRealm(t *testing.T) {
	p := New("")
	cn := &auth.Auth{UID: "cn-user", Nickname: "国内号", AccessToken: "at-cn"}
	g := &auth.Auth{UID: "global-user", Nickname: "国际号", AccessToken: "at-g", Domain: "codebuddy.ai"}
	p.Add(cn)
	p.Add(g)

	// 强制国内号
	if _, err := p.PinAccount("cn-user"); err != nil {
		t.Fatal(err)
	}
	// 强制国际号
	if _, err := p.PinAccount("global-user"); err != nil {
		t.Fatal(err)
	}

	pinnedMap := p.PinnedUIDs()
	if pinnedMap["cn"] != "cn-user" || pinnedMap["global"] != "global-user" {
		t.Fatalf("unexpected pinned map: %+v", pinnedMap)
	}

	// 选号域隔离测试
	pickedCN := p.PickExcludingForRealm(nil, "", "cn")
	if pickedCN == nil || pickedCN.UID != "cn-user" {
		t.Fatalf("expected cn-user, got %v", pickedCN)
	}

	pickedGlobal := p.PickExcludingForRealm(nil, "", "global")
	if pickedGlobal == nil || pickedGlobal.UID != "global-user" {
		t.Fatalf("expected global-user, got %v", pickedGlobal)
	}

	// 一键清除全部
	if err := p.UnpinAccount("all"); err != nil {
		t.Fatal(err)
	}
	if len(p.PinnedUIDs()) != 0 {
		t.Fatalf("expected all unpinned, got %+v", p.PinnedUIDs())
	}
}
