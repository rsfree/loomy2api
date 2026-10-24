// abort.go 「不是账号问题」的上游失败：调用方中途断开。
//
// 线上事实（2026-10-24 复盘 62h 台账）：629 条「连接类 502」里有 605 条的
// error 是 `Post "…/images/generations": context canceled`，耗时 p25–p90
// 全卡在 900.0s —— 不是我们自己的 ctx 到期（那样 Go 报的是 context deadline
// exceeded），而是**调用方在 900s 整断开**（聚合层超时/用户取消）。
//
// 旧逻辑把它当普通连接故障：ReportFailure(acc, 0, …) → Classify 判
// FaultUnknown → 给账号上 CooldownFallback 冷却；而且循环继续换号，每次尝试
// 再罚一个号 ⇒ 一次客户端放弃最多冻掉 MaxRetries+1 个账号。排队高峰里
// 「越忙 → 断得越多 → 冷却越多」会自我放大。
//
// ⚠️ 边界（0.0.34 的教训）：只有**我们的尝试上限**到期才算超时；
// TLS 握手超时（transport 里 15s）、dial i/o timeout 这些是**单次连接故障**，
// 换号换出口往往就能过，必须继续走「罚号 + 换号重试」。0.0.34 把
// net.Error.Timeout() 也算成超时，上线 25 分钟内 4 个请求在 15s 就被判死成
// 504（本该换号重试后成功）。
package server

import (
	"context"
	"errors"
	"net/http"
)

const (
	// statusClientClosed nginx 口径：客户端在拿到响应前断开。
	statusClientClosed = 499

	abortClientGoneBody = `{"error":{"message":"客户端已断开，生成结果请稍后用同一请求重试","type":"client_closed_request","param":null,"code":499}}`
)

// errClientGone 调用方在响应前断开；没有重试价值，必须立刻停。
var errClientGone = errors.New("客户端已断开")

// clientGone 调用方是否已经断开（请求上下文被取消或超时）。
func clientGone(r *http.Request) bool {
	return r != nil && r.Context().Err() != nil
}

// attemptCapExpired 本次尝试是否被**我们自己的上限**掐断。
//
// 只看传进来的 ctx.Err()，而且必须在 cancel() 之前取：cancel 之后它恒为
// context.Canceled，和「客户端断开」长得一模一样。
// 不能用 errors.Is(err, context.DeadlineExceeded) 或 net.Error.Timeout() 代替 ——
// 那两者会把 TLS 握手超时、dial 超时也吞进来（见文件头的 0.0.34 教训）。
func attemptCapExpired(attemptErr error) bool {
	return attemptErr == context.DeadlineExceeded
}

// abortStatus 把「客户端断开」映射成 499；其余维持 502。
func abortStatus(err error) int {
	if errors.Is(err, errClientGone) {
		return statusClientClosed
	}
	return http.StatusBadGateway
}
