package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		// 【2026-10-21 修正】原断言写 false，理由是"规范要求精确"——**那个理由是错的**。
		// RFC 7235 §2.1："The scheme name is case-insensitive"。不少 SDK / HTTP 库
		// 默认就发小写 `bearer`，判 401 会让用户觉得"密钥明明对却认证失败"。
		// 方案名后多空格（`Bearer  key`）同样是常见形态，一并容忍。
		{"前缀大小写不同（RFC 7235 方案名不区分大小写）", "sk-abc123", "bearer sk-abc123", true},
		{"方案名后多余空格", "sk-abc123", "Bearer  sk-abc123", true},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// 密钥轮换：过渡钥（api_key_previous）在主钥之外也被接受，且能区分"用的是哪一个"。
// 为什么需要：轮换只认新钥时，所有把旧钥缓存在内存里的已启动客户端会立刻 401。
func TestVerifyBearerTransition(t *testing.T) {
	mk := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}
	const primary, previous = "sk-wb2a-NEWBYTE", "sk-wb2a-OLDBYTE"

	if ok, prev := VerifyBearerTransition(mk(primary), primary, previous); !ok || prev {
		t.Errorf("主钥应通过且不算过渡钥，得到 ok=%v prev=%v", ok, prev)
	}
	if ok, prev := VerifyBearerTransition(mk(previous), primary, previous); !ok || !prev {
		t.Errorf("过渡钥应通过且标记为过渡钥，得到 ok=%v prev=%v", ok, prev)
	}
	// 清空过渡钥 = 轮换完成 → 旧钥必须立刻失效
	if ok, _ := VerifyBearerTransition(mk(previous), primary, ""); ok {
		t.Error("过渡钥已清空时旧钥不该通过（轮换未真正完成）")
	}
	// 未知钥
	if ok, _ := VerifyBearerTransition(mk("sk-wb2a-WRONG"), primary, previous); ok {
		t.Error("未知钥不该通过")
	}
	// 无 Authorization 头
	if ok, _ := VerifyBearerTransition(mk(""), primary, previous); ok {
		t.Error("缺头不该通过")
	}
	// primary 为空 = 不鉴权（与 VerifyBearer 同语义）
	if ok, prev := VerifyBearerTransition(mk(""), "", ""); !ok || prev {
		t.Errorf("primary 为空应放行且不算过渡钥，得到 ok=%v prev=%v", ok, prev)
	}
	// previous == primary 属无效配置（normalize 会清掉；这里也不该被当成"过渡钥命中"）
	if ok, prev := VerifyBearerTransition(mk(primary), primary, primary); !ok || prev {
		t.Errorf("previous==primary 应视作主钥命中，得到 ok=%v prev=%v", ok, prev)
	}
}
