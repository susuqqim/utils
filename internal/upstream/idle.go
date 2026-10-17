// idle.go 聊天 SSE 流中空闲监控：活跃吐数据续命不掐，静默超过阈值才断流（释放租约）。
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
	"workbuddy2api/internal/gosafe"
)

// idleMonitoringBody 包在聊天 SSE body 外层：
// 每次读到底层数据（n>0）就刷新 lastRead；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
type idleMonitoringBody struct {
	rc       io.ReadCloser
	mu       sync.Mutex
	lastRead time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close 停掉后台 goroutine、取消请求 context、关闭底流，保证无泄漏。
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody 若 idle<=0 直接返回原底流（禁用空闲监控）；
// 否则包上流中空闲监控。计时从返回 body 之后开始——首字节阶段由
// Transport.ResponseHeaderTimeout 管，这里不抢跑。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	// 监控协程兜底：panic 只应中断本次请求（cancel），不能把进程带走。
	// 这里是**请求级**看门狗（随请求生灭、以 ctx 生命周期结束），不是长驻循环：
	// 探测一次 panic 即取消该次请求，由调用方按失败处理即可 —— 故用 DoErr 顶层兜底，
	// 不套 `gosafe.Loop`（AGENTS.md 的"长驻循环按迭代兜底"规则针对 ticker/for-select 形态）。
	go gosafe.DoErr("idle monitor", func(error) { cancel() }, func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	})
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
