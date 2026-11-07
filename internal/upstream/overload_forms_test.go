package upstream

import "testing"

// 上游信封有多种形态：顶层 code、error.code、data.code。
// 11134 在 data 里时也必须识别为"供应商过载"（审计 R2-core ①）。
func TestIsProviderOverloadEnvelopeForms(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11134,"msg":"overloaded"}`, true},
		{`{"code":"11134"}`, true},
		{`{"error":{"code":11134}}`, true},
		{`{"data":{"code":11134,"msg":"x"}}`, true},
		{`{"code":11102,"extra":{"code":11134}}`, false},
		{`{"code":11102,"msg":"no such model"}`, false},
		{`{"code":11102,"data":{"code":11134}}`, false},
		{`{"error":{"code":11102},"data":{"code":11134}}`, false},
		{`{"code":" 11134 "}`, true},
		{`{"code":11134.0}`, true},
	}
	for _, c := range cases {
		if got := IsProviderOverload(c.body); got != c.want {
			t.Errorf("IsProviderOverload(%s)=%v want %v", c.body, got, c.want)
		}
	}
}
