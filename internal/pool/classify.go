// classify.go 上游错误分类与账号处置语义。
package pool

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 错误分类。
type Fault int

const (
	FaultUnknown    Fault = iota
	FaultQuota            // 额度/积分耗尽 → 硬冷却
	FaultModelLimit       // 模型级限流 → 只锁该模型
	FaultRateLimit        // 频控 → 软冷却
	FaultNotFound         // 404 → 模型级失效台账（不罚账号，见 deadmodel.go）
	FaultSession          // 登录态失效 → 需重登
	FaultServer           // 5xx → 喂熔断
	FaultClient           // 客户端问题 → 不罚账号
)

var (
	reQuotaWords = []string{
		"积分不足", "余额不足", "额度不足", "额度已用完", "insufficient", "quota exceeded",
		"points", "balance",
	}
	reSessionWords = []string{
		"session not found", "session 失效", "session invalid", "unauthorized",
		"invalid token", "登录已过期", "登录已失效", "登录失效", "登录态", "token expired", "401", "100002", "020002",
	}
	reRateWords = []string{
		"rate limit", "too many requests", "frequency", "限流", "请求过于频繁", "qps",
	}
	reModelLimit = regexp.MustCompile(`(?i)(model[^,;]{0,24}(limit|exceed|unavailable)|模型[^,;]{0,12}(限流|超限|不可用)|queue\.full|6021)`)
	reResetAt    = regexp.MustCompile(`(?:将在|重置于|reset at|until)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(?::\d{2})?)`)
)

// Classify 判定一次失败的错误类别。判定优先级：
// 额度耗尽 → 登录态失效 → 模型级限流 → 限流文案 → 状态码兜底。
func Classify(status int, body string) Fault {
	lower := strings.ToLower(body)

	for _, w := range reQuotaWords {
		if strings.Contains(lower, w) && status >= 400 {
			// 「points」太泛（如 points_consumed 回显），只在 402/403/429 下当真。
			if w == "points" || w == "balance" {
				if status != 402 && status != 403 && status != 429 {
					continue
				}
			}
			return FaultQuota
		}
	}
	if status == 402 {
		return FaultQuota
	}

	if status == 401 {
		return FaultSession
	}
	for _, w := range reSessionWords {
		if w == "401" {
			continue
		}
		if strings.Contains(lower, w) {
			return FaultSession
		}
	}

	if status == 429 || status == 403 {
		if reModelLimit.MatchString(body) {
			return FaultModelLimit
		}
	}
	if reModelLimit.MatchString(body) && status >= 400 {
		return FaultModelLimit
	}

	if status == 429 {
		return FaultRateLimit
	}
	for _, w := range reRateWords {
		if strings.Contains(lower, w) {
			return FaultRateLimit
		}
	}

	switch {
	case status == 404:
		return FaultNotFound
	case status >= 500:
		return FaultServer
	case status >= 400:
		return FaultClient
	}
	return FaultUnknown
}

// ResetAtFromBody 从错误文案里解析上游给出的重置墙钟（解析失败返回 0）。
func ResetAtFromBody(body string) float64 {
	m := reResetAt.FindStringSubmatch(body)
	if len(m) < 2 {
		return 0
	}
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, m[1], time.Local); err == nil {
			return float64(t.Unix())
		}
	}
	return 0
}

// NextDailyReset 次日 00:00 的时间戳（额度耗尽硬冷却终点）。
//
// ⚠️ 是**自然日 24:00**，不是本地 04:00：上游的每日赠送额度按自然日重置
// （实测 0 点后就能继续用），按 04:00 冷却会让 23:50 烧空额度的账号白等到次日
// 04:00 —— 白丢 4 小时可用时间。这个口径来自 dsh-providers-hub 对同一个上游的
// 实测结论（Gitee IKL80H），与 WorkBuddy 的 04:00 只是巧合、不可互抄。
func NextDailyReset() float64 {
	now := time.Now()
	return float64(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, 1).Unix())
}

// FaultMessage 从上游错误体里取一句人能读的原因（台账 / 面板展示用）。
//
// 直接存整段 body 会把 "{\"error\":{\"code\":404,...}}" 原样塞进面板台账里，
// 既难认也没必要；取不到就退回截断的原文，至少不丢信息。
func FaultMessage(body string) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Desc    string `json:"desc"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &env); err == nil {
		for _, s := range []string{env.Error.Message, env.Desc, env.Message} {
			if s != "" {
				return s
			}
		}
	}
	return truncate(body, 120)
}

// AccountFault 该分类是否算「账号自身的问题」（换号可能有效）。
func AccountFault(f Fault) bool {
	switch f {
	case FaultQuota, FaultSession, FaultRateLimit, FaultModelLimit:
		return true
	}
	return false
}

// FatalRequest 该分类不该换号重试。
//
// 两类都算：客户端请求本身有问题（FaultClient），以及**换号也不会好**的
// 模型级 404（FaultNotFound）。后者尤其要停：旧实现把 404 当账号故障去罚账号
// 并换下一个号，一个打错模型名的请求会接连冻掉池里所有账号，最后还是 404。
func FatalRequest(f Fault) bool {
	return f == FaultClient || f == FaultNotFound
}

// StatusFromBody 尽力从上游错误体里取 HTTP 状态（有些网关把状态塞进 body）。
func StatusFromBody(body string) int {
	for _, key := range []string{`"status":`, `"code":`} {
		if i := strings.Index(body, key); i >= 0 {
			rest := strings.TrimLeft(body[i+len(key):], " ")
			end := 0
			for end < len(rest) && (rest[end] >= '0' && rest[end] <= '9') {
				end++
			}
			if end > 0 {
				if n, err := strconv.Atoi(rest[:end]); err == nil && n >= 100 && n < 600 {
					return n
				}
			}
		}
	}
	return 0
}
