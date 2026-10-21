// util.go bridge 包内部工具。
package bridge

import (
	"fmt"
	"time"
)

// timeNow 当前 unix 秒。
func timeNow() int64 { return time.Now().Unix() }

// timeOf 把任意数值/字符串转成 unix 秒（缺省回落当前时间）。
func timeOf(v any) int64 {
	switch t := v.(type) {
	case float64:
		if t > 0 {
			return int64(t)
		}
	case int64:
		if t > 0 {
			return t
		}
	case int:
		if t > 0 {
			return int64(t)
		}
	case string:
		var f float64
		if _, err := fmt.Sscanf(t, "%f", &f); err == nil && f > 0 {
			return int64(f)
		}
	}
	return time.Now().Unix()
}
