package gosafe

import (
	"errors"
	"sync"
	"testing"
)

// Do 必须吞掉 panic 并让进程存活（这就是它的全部意义）。
func TestDoRecoversPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		Do("test label", func() { panic("boom") })
	}()
	<-done // 没被 panic 打断就说明 recover 生效
}

// Do 不得吞掉正常返回路径（无 panic 时照常执行完）。
func TestDoRunsFnWhenNoPanic(t *testing.T) {
	ran := false
	Do("ok", func() { ran = true })
	if !ran {
		t.Fatal("fn 未被执行")
	}
}

// DoErr 必须把 panic 变成错误交给等待方，否则等待方永久阻塞。
func TestDoErrDeliversPanicAsError(t *testing.T) {
	var got error
	done := make(chan struct{})
	go func() {
		defer close(done)
		DoErr("probe", func(err error) { got = err }, func() { panic(errors.New("x")) })
	}()
	<-done
	if got == nil {
		t.Fatal("panic 未转成 error（等待方会永久阻塞）")
	}
}

// Loop 的核心语义：**单次迭代 panic 不终止循环**（这是它与 Do 的关键区别——
// Do 包住整个循环时，一次 panic 会让"落盘/GC/跨日重置"这类长驻服务永久停摆且无迹象）。
func TestLoopKeepsLoopAliveAfterPanic(t *testing.T) {
	iters := 0
	for i := 0; i < 5; i++ {
		gosafe_LoopForTest(&iters, i)
	}
	if iters != 5 {
		t.Fatalf("循环被 panic 打断：只跑了 %d/5 轮", iters)
	}
}

func gosafe_LoopForTest(iters *int, i int) {
	Loop("test iteration", func() {
		*iters++
		if i == 2 {
			panic("boom in iteration 2")
		}
	})
}

// Loop 内 panic 时，迭代体内的 defer 必须照常执行（持锁场景靠它 Unlock——
// Go 的 mutex 不会因 panic 自动释放，漏了就是永久死锁）。
func TestLoopReleasesLocksViaDefer(t *testing.T) {
	var mu sync.Mutex
	unlocked := false
	Loop("lock iteration", func() {
		mu.Lock()
		defer func() { mu.Unlock(); unlocked = true }()
		panic("boom while holding lock")
	})
	if !unlocked {
		t.Fatal("panic 时 defer 未执行：锁会被永久占用（死锁）")
	}
	if !mu.TryLock() {
		t.Fatal("锁未被释放")
	}
	mu.Unlock()
}
