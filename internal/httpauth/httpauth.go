// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 统一成一份实现的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处
// 鉴权必须完全同口径，各自复制一份既容易漂移，又都带计时侧信道。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// Verify 校验请求头是否携带正确的密钥。
//
// keys 为空表示「未启用鉴权」，恒返回 true。比较用 SHA-256 摘要 +
// subtle.ConstantTimeCompare：常量时间、长度差异被吸收进摘要、摘要不可逆。
// 同时接受 Authorization: Bearer 与 x-api-key（Anthropic 客户端用后者）。
func Verify(r *http.Request, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	token := ""
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, bearerPrefix) {
		token = authz[len(bearerPrefix):]
	} else if k := r.Header.Get("x-api-key"); k != "" {
		token = k
	}
	ok := false
	for _, key := range keys {
		if subtle.ConstantTimeCompare(digest(token), digest(key)) == 1 {
			ok = true
		}
	}
	if !ok {
		// 保持耗时形状一致：即便没命中也要走完比较。
		subtle.ConstantTimeCompare(digest(""), digest(keys[0]))
	}
	return ok
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
