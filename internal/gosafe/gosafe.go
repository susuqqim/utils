// Package gosafe 为"分离出去跑"的后台 goroutine 提供统一的 panic 兜底。
//
// 为什么必须有：Go 里 **goroutine 内的 panic 不会被 net/http 的 handler recover 兜住**，
// 也无法被其它 goroutine 的 recover 拦截 —— 它会直接终止整个进程。网关是长期驻留的
// 多账号服务：一个后台任务（模型探测 / 签到 / 任务队列 / 落盘 / 会话镜像）里的
// nil 解引用、越界或并发 map 写，就会让**所有在途请求同时中断**，而且现场只剩一个
// 崩溃栈，用户侧表现为"网关忽然没了"。
//
// 因此凡是"扔到后台自己跑"的工作，顶层一律包一层 Do：单个任务失败只应影响那个任务。
//
// 这不是"消灭 bug"的许可证 —— panic 仍会打出一条带栈的 ERR 日志（标签 + 栈），
// 便于事后定位；只是把故障半径从"整个进程"缩到"那一次后台任务"。
package gosafe

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
)

// Do 在后台 goroutine 中执行 fn，并把 panic 降级为一条带栈的 ERR 日志。
//
// 用法：go gosafe.Do("taskcenter worker", func() { ... })
func Do(label string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			logPanic(label, p)
		}
	}()
	fn()
}

// Loop 在**长驻服务循环的每次迭代**内执行 fn：panic 只终止本次迭代，循环继续。
//
// 为什么不能用 Do 代替：Do 包住的是整个 goroutine —— 循环体内一次 panic 被 recover 后，
// 整个 goroutine 就结束了，于是"落盘 / GC / 会话镜像 / 跨日重置"这类**长驻循环会永久停摆**，
// 而且没有任何崩溃迹象（比直接退出更隐蔽）。所以：
//
//	长驻循环 → go Do(...) { for { Loop(label, func(){ 单次迭代 }) } }   ← 顶层+迭代各兜一层
//	一次性任务 → go Do(label, task)
//	调用方在等结果 → go DoErr(label, onPanic, task)
//
// ⚠️ 迭代体内**持锁**时务必用 `defer Unlock()`：Go 的 mutex 不会因为 panic 自动释放，
// 否则 recover 之后锁永久被占，整个组件死锁（比崩溃更难查）。
// 返回 true 表示本轮发生了 panic（已被兜住）。调用方据此决定是否加退避
// （否则"每轮都 panic"会变成 100% CPU 紧循环）。
func Loop(label string, fn func()) (panicked bool) {
	defer func() {
		if p := recover(); p != nil {
			panicked = true
			logPanic(label+" (iteration recovered, loop continues)", p)
		}
	}()
	fn()
	return false
}

// DoErr 同 Do，但把 panic 也交给 onPanic（用于"调用方在等一个结果"的 goroutine：
// 必须把 panic 变成一条错误结果，否则等待方会永久阻塞）。onPanic 自身不应阻塞。
func DoErr(label string, onPanic func(error), fn func()) {
	defer func() {
		if p := recover(); p != nil {
			logPanic(label, p)
			if onPanic != nil {
				onPanic(panicError{label: label, val: p})
			}
		}
	}()
	fn()
}

type panicError struct {
	label string
	val   any
}

func (e panicError) Error() string {
	return "panic in " + e.label + ": " + fmt.Sprint(e.val)
}

// 日志节流：panic 栈每次 1~4KB，若某个"每请求一 panic"的热点（例如 SSE 读循环）持续触发，
// 无节流会把日志文件/面板 Ring 刷爆。口径与 usage/pool 的落盘失败节流一致：
// 首次全量（含栈），之后每 100 次打一条一行摘要，便于"知道还在发生"又不淹没日志。
const panicLogEvery = 100

var (
	panicMu    sync.Mutex
	panicCount = map[string]int{}
)

func logPanic(label string, p any) {
	panicMu.Lock()
	panicCount[label]++
	n := panicCount[label]
	panicMu.Unlock()
	if n == 1 || n%panicLogEvery == 0 {
		log.Printf("ERR: [gosafe] panic in %s (第 %d 次): %v\n%s", label, n, p, debug.Stack())
		return
	}
	log.Printf("ERR: [gosafe] panic in %s (第 %d 次，栈已省略；每 %d 次打印一次完整栈): %v",
		label, n, panicLogEvery, p)
}
