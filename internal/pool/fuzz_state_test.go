package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// `data/state.json` 是磁盘上的状态文件：可能被写坏（掉电/半截写）、被手工编辑、
// 或被降级/升级版本互写。加载它**绝不能 panic**（否则网关起不来或永久丢状态）。
// 用例同时覆盖"加载 → 使用 → 落盘"整条链路。
// 用法： go test ./internal/pool/ -run FuzzStateLoad -fuzz FuzzStateLoad -fuzztime 60s
func FuzzStateLoad(f *testing.F) {
	seeds := [][]byte{
		[]byte(``),
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(`{"accounts":{}}`),
		[]byte(`{"accounts":{"u1":{"credits":100}}}`),
		[]byte(`{"accounts":{"u1":{"credits":-1,"err_count":-2,"soft_streak":-3}}}`),
		[]byte(`{"accounts":{"u1":{"packages":[{"remain":-1,"expire":"0001-01-01T00:00:00Z"}]}}}`),
		[]byte(`{"accounts":{"u1":{"daily_marks":{"makeup":"2026-10-03"}}}}`),
		[]byte(`{"accounts":{"u1":{"cool_kind":"bogus","until":"9999-99-99T99:99:99Z"}}}`),
		[]byte(`{"accounts":{"":{"credits":1}}}`),
		[]byte(`{"accounts":{"u1":{"token_usage":{"request_count":-1,"last_tokens_per_second":1e309}}}}`),
		[]byte("{\"accounts\":{\"u1\":{\"credits\":1}}}garbage"),
		[]byte(`{"saved_at":"not-a-time","accounts":{"u1":null}}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "state.json")
		if err := os.WriteFile(fp, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		p := New(fp)
		p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()})
		p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: "workbuddy.ai"})
		// 读路径
		_ = p.List()
		_, _ = p.Status("u1")
		_ = p.DailyMarkDoneToday("u1", "makeup")
		_ = p.AvailableUIDsForRealm("cn")
		_ = p.Pick()
		// 写路径（含把任意加载来的状态再落盘一次）
		p.NoteDailyMark("u1", "makeup", "2026-10-03")
		p.NoteSuccess("u1")
		p.NoteError("u1")
		p.Flush()
		// 再加载一次（模拟重启）
		p2 := New(fp)
		p2.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()})
		_ = p2.List()
	})
}
