package server

import (
	"net/http"
	"testing"
)

// resolveCacheConvID 三级优先级（移植上游 20254d8）：body → 头 → **会话 key**（回退是关键）。
func TestResolveCacheConvID(t *testing.T) {
	h := func(v string) http.Header {
		hh := http.Header{}
		if v != "" {
			hh.Set("X-Conversation-ID", v)
		}
		return hh
	}
	cases := []struct{ name, body, header, sess, want string }{
		{"body 优先", `{"conversation_id":"cb"}`, "ch", "s1", "cb"},
		{"body 空用头", `{}`, "ch", "s1", "ch"},
		{"两源皆空 ⇒ 回退会话 key", `{}`, "", "s1", "s1"},
		{"头纯空白视作空", `{}`, "   ", "s1", "s1"},
		{"全空 ⇒ 空（由注入器定值兜底）", `{}`, "", "", ""},
	}
	for _, c := range cases {
		if got := resolveCacheConvID([]byte(c.body), h(c.header), c.sess); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
