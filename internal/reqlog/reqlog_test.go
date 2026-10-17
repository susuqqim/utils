package reqlog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSanitizeIP(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":         "1.2.3.0/24",
		" 10.20.30.40 ":   "10.20.30.0/24",
		"2001:db8:1:2::3": "2001:db8:1::/48",
		"":                "",
		"not-an-ip":       "not-an-ip",
	}
	for in, want := range cases {
		if got := SanitizeIP(in); got != want {
			t.Errorf("SanitizeIP(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSanitizeUA(t *testing.T) {
	if got := SanitizeUA("a\x00b\x1fc"); got != "abc" {
		t.Errorf("控制字符未剥离: %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if n := len([]rune(SanitizeUA(long))); n != 120 {
		t.Errorf("UA 应截断到 120，实得 %d", n)
	}
	if got := UID8("0123456789abcdef"); got != "01234567" {
		t.Errorf("UID8=%q", got)
	}
}

func TestRingCapacityAndOrder(t *testing.T) {
	r := NewRing(3)
	base := time.Now()
	for i := 0; i < 5; i++ {
		r.Add(Entry{TS: base.Add(time.Duration(i) * time.Second), Path: string(rune('a' + i))})
	}
	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("容量 3 应只留 3 条，实得 %d", len(got))
	}
	// 覆盖后应剩 c/d/e，且按时间升序
	if got[0].Path != "c" || got[2].Path != "e" {
		t.Errorf("环形覆盖语义错误: %v %v %v", got[0].Path, got[1].Path, got[2].Path)
	}
	// 乱序写入也要按时间升序返回
	r2 := NewRing(3)
	r2.Add(Entry{TS: base.Add(3 * time.Second), Path: "late"})
	r2.Add(Entry{TS: base, Path: "early"})
	if s := r2.Snapshot(); s[0].Path != "early" || s[1].Path != "late" {
		t.Errorf("快照应按时间升序: %v", s)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := NewRing(64)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Add(Entry{TS: time.Now(), Path: "p"})
			}
			_ = r.Snapshot()
		}(i)
	}
	wg.Wait()
	if n := len(r.Snapshot()); n != 64 {
		t.Errorf("并发写后应保持满容量 64，实得 %d", n)
	}
}

func TestArchiveOrderingAndRetention(t *testing.T) {
	dir := t.TempDir()
	a := NewArchive(dir, time.Hour, 1<<20)
	base := time.Now().Add(-30 * time.Minute)
	// 故意**乱序**追加，并让 mtime 与事件时间不一致（验证按事件时间排序）
	for _, off := range []int{3, 0, 2, 1} {
		if err := a.Append(Entry{TS: base.Add(time.Duration(off) * time.Minute), Path: string(rune('a' + off))}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.Read(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("应有 4 条，实得 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].TS.Before(got[i-1].TS) {
			t.Fatalf("读取必须按**事件时间**升序（上游 7d462bb），第 %d 条乱序", i)
		}
	}
	if got[0].Path != "a" || got[3].Path != "d" {
		t.Errorf("顺序错误：%v…%v", got[0].Path, got[3].Path)
	}
	if l, _ := a.Read(time.Time{}, 2); len(l) != 2 || l[1].Path != "d" {
		t.Errorf("limit 应取最近 2 条（仍升序），实得 %v", l)
	}
	// 保留期回收：造一个"很旧 mtime"的文件，应被删
	old := filepath.Join(dir, "req-2000-01-01.ndjson")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(old, past, past)
	_ = a.Append(Entry{TS: time.Now(), Path: "x"})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("超过保留期的归档文件应被回收")
	}
	files, bytes := a.Stats()
	if files == 0 || bytes == 0 {
		t.Errorf("Stats 应报告非空: files=%d bytes=%d", files, bytes)
	}
}
