package upstream

import "testing"

// 回归（2026-10-20，对照 hub #499/#505）：11158「该用户类型无权使用此模型」等**模型级**错误码
// 不得罚账号/熔断整池——换模型就好，账号是健康的。
func TestModelLevelCodesDoNotPenalizeAccount(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"11158 免费账号无权", 403, `{"code":11158,"msg":"model not allowed for user type"}`},
		{"11158 400 形态", 400, `{"code":11158,"msg":"model not allowed for user type"}`},
		{"11134 提供方不可用 500", 500, `{"code":11134,"msg":"model provider temporarily unavailable"}`},
		{"11150 模型不可用", 400, `{"code":11150,"msg":"model not available"}`},
		{"11102 已知形态", 400, `{"code":11102,"msg":"model not found"}`},
		{"11129 参数拒绝", 400, `{"code":11129,"msg":"invalid function call parameters"}`},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != ErrModelBlocked {
			t.Errorf("%s: Classify(%d)=%v want ErrModelBlocked", c.name, c.status, got)
		}
	}
	// 真正的 5xx（无模型级码）仍应喂熔断
	if got := Classify(500, `{"code":500,"msg":"internal error"}`); got != ErrServer {
		t.Errorf("普通 500 应仍归 ErrServer，实得 %v", got)
	}
	// 账号级限制不能被模型级码逻辑抢走
	if got := Classify(403, `{"code":11140,"msg":"request illegal"}`); got != ErrAccountFault {
		t.Errorf("11140 应仍归 ErrAccountFault，实得 %v", got)
	}
}

func TestHasModelLevelCode(t *testing.T) {
	if !hasModelLevelCode(`{"code":11158}`) {
		t.Error("11158 应命中")
	}
	if hasModelLevelCode(`{"code":11140}`) {
		t.Error("11140 不是模型级码")
	}
	if hasModelLevelCode(`{"code":11128}`) {
		t.Error("11128 是通道校验，不是模型级码")
	}
}
