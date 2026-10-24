// abort.go 「不是账号问题」的上游失败：调用方断开与连接超时。
//
// 线上事实（2026-10-24 复盘 62h 台账）：629 条「连接类 502」里有 605 条的
// error 是 `Post "…/images/generations": context canceled`，耗时 p25–p90
// 全卡在 900.0s。如果是我们自己的 ctx 到期，Go 报的是 context deadline
// exceeded（errors.Is 能捕获、旧代码会回 504），所以这 605 条不是超时，
// 而是**调用方在 900s 整断开**（聚合层超时/用户取消）。
//
// 旧逻辑把它当普通连接故障：ReportFailure(acc, 0, …) → Classify 判
// FaultUnknown → 给账号上 CooldownFallback 冷却；而且循环会继续换号重试，
// 每次尝试再罚一个号 ⇒ 一次客户端放弃最多冻掉 MaxRetries+1 个账号。
// 排队高峰里这种「自我放大」比上游故障本身更伤吞吐：越忙 → 断得越多 →
// 冷却越多 → 剩下的号越忙。
//
// 因此这里把两类中断单独识别出来：不罚账号、不写 502，也不再重试。
package server

import (
	"context"
	"errors"
	"net"
	"net/http"
)

const (
	// statusClientClosed nginx 口径：客户端在拿到响应前断开。
	statusClientClosed = 499

	abortClientGoneBody     = `{"error":{"message":"客户端已断开，生成结果请稍后用同一请求重试","type":"client_closed_request","param":null,"code":499}}`
	abortConnectTimeoutBody = `{"error":{"message":"上游连接超时，请稍后重试","type":"timeout_error","param":null,"code":504}}`
)

var (
	// errClientGone 调用方在响应前断开；没有重试价值，必须立刻停。
	errClientGone = errors.New("客户端已断开")
	// errConnectTimeout 上游连接层超时（TLS 握手、读头超时）：可换号再试，但不是账号故障。
	errConnectTimeout = errors.New("上游连接超时")
)

// clientGone 调用方是否已经断开（请求上下文被取消或超时）。
func clientGone(r *http.Request) bool {
	return r != nil && r.Context().Err() != nil
}

// attemptTimedOut 这次失败是不是「超时」。
//
// attemptErr 是那一次尝试自己的 ctx.Err()，**必须在 cancel() 之前取**：
// cancel 之后它恒为 context.Canceled，和「客户端断开」长得一模一样。
// 三种措辞都要认：ctx 到期（DeadlineExceeded）、url.Error 包装的到期、
// 以及 http.Client.Timeout 的 net.Error.Timeout()。
func attemptTimedOut(err, attemptErr error) bool {
	if attemptErr == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// abortStatus 把中断类错误映射成对外状态码；其余维持 502（真连接故障）。
func abortStatus(err error) int {
	switch {
	case errors.Is(err, errClientGone):
		return statusClientClosed
	case errors.Is(err, errConnectTimeout):
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
