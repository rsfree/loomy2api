// util.go pool 包内部小工具（避免跨包依赖 fmt 之外的重量级工具）。
package pool

import (
	"fmt"
	"strconv"
	"time"
)

func nowF() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string { return strconv.Itoa(n) }

func strOf(vals ...any) string {
	for _, v := range vals {
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			return fmt.Sprintf("%.0f", t)
		case int64:
			return strconv.FormatInt(t, 10)
		case int:
			return strconv.Itoa(t)
		}
	}
	return ""
}

func int64Of(vals ...any) int64 {
	for _, v := range vals {
		switch t := v.(type) {
		case float64:
			return int64(t)
		case int64:
			return t
		case int:
			return int64(t)
		case string:
			if t == "" {
				continue
			}
			if n, err := strconv.ParseInt(t, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// numOf 取浮点数值（state.json 读回来是 float64；兼容字符串与整型）。
func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return 0
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}
