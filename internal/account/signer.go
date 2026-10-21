// Package account 讯飞账号服务客户端：签名、RSA 密码加密、登录与用户信息。
//
// 协议逆向自 Loomy 桌面客户端（Electron 0.9.38），详见 docs/PROTOCOL.md。
// 关键事实：
//   - 登录态就是要发给模型网关的 Bearer token，没有 refresh token；
//   - 有效期 14 天，过期只能重新登录 —— 因此「存手机号 + 密码」是无人值守的前提；
//   - rcode 是服务端签发的 nonce，不是验证码，所以密码登录可全自动。
package account

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// extraEscapes RFC3986 子分隔符中 url.QueryEscape 不会转义、但服务端要转义的字符。
var extraEscapes = map[string]string{
	"!": "%21", "'": "%27", "(": "%28", ")": "%29", "*": "%2A",
}

// Escape 按服务端口径做 RFC3986 编码（逐段）。
func Escape(s string) string {
	out := url.QueryEscape(s) // QueryEscape 用 + 表示空格，需改回 %20
	out = strings.ReplaceAll(out, "+", "%20")
	for raw, enc := range extraEscapes {
		out = strings.ReplaceAll(out, raw, enc)
	}
	return out
}

func EscapePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s != "" {
			segs[i] = Escape(s)
		}
	}
	return strings.Join(segs, "/")
}

func EscapeQuery(q map[string]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, Escape(k)+"="+Escape(q[k]))
	}
	return strings.Join(parts, "&")
}

// ContentMD5 base64(md5(body))，空 body 返回空串。
func ContentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func canonicalized(headers map[string]string) (string, string) {
	picked := map[string]string{}
	for k, v := range headers {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-") {
			picked[lk] = strings.TrimSpace(v)
		}
	}
	if len(picked) == 0 {
		return "", ""
	}
	keys := make([]string, 0, len(picked))
	for k := range picked {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canon strings.Builder
	for _, k := range keys {
		canon.WriteString(k + ":" + picked[k] + "\n")
	}
	return strings.Join(keys, ";"), strings.TrimSuffix(canon.String(), "\n")
}

// StringToSign 拼出待签名串（固定 9 行，末两行无自定义头时为空串但不能省略）。
func StringToSign(method, path string, query map[string]string, body, contentType, date, nonce string, custom map[string]string) string {
	signed, canonical := canonicalized(custom)
	lines := []string{
		strings.ToUpper(method),
		EscapePath(path),
		EscapeQuery(query),
		ContentMD5(body),
		contentType,
		date,
		nonce,
		signed,
		canonical,
	}
	return strings.Join(lines, "\n")
}

// Sign base64(HMAC-SHA1(secret, stringToSign))。
func Sign(secret string, method, path string, query map[string]string, body, contentType, date, nonce string, custom map[string]string) string {
	sts := StringToSign(method, path, query, body, contentType, date, nonce, custom)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(sts))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// BuildHeaders 生成一次账号服务请求的完整头集合。
func BuildHeaders(akID, akSecret, method, path, body string, custom map[string]string) map[string]string {
	date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	nonce := NewUUID()
	const contentType = "application/json"
	sig := Sign(akSecret, method, path, nil, body, contentType, date, nonce, custom)
	headers := map[string]string{
		"Authorization": "account " + akID + ":" + sig,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  contentType,
	}
	if md5s := ContentMD5(body); md5s != "" {
		headers["Content-MD5"] = md5s
	}
	for k, v := range custom {
		headers[k] = v
	}
	return headers
}

// ------------------------------------------------------------------ RSA

// RSAEncrypt 用服务端下发的 DER 公钥做 PKCS#1 v1.5 加密，返回 base64 密文。
func RSAEncrypt(pubkeyB64, plaintext string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(padB64(pubkeyB64))
	if err != nil {
		return "", fmt.Errorf("公钥 base64 解码失败: %w", err)
	}
	pub, err := ParsePublicKey(der)
	if err != nil {
		return "", err
	}
	cipher, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plaintext))
	if err != nil {
		return "", fmt.Errorf("RSA 加密失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}

// padB64 补齐缺失的 base64 padding（Node 的 Buffer.from 容忍，Go 不容忍）。
func padB64(s string) string {
	if m := len(s) % 4; m != 0 {
		return s + strings.Repeat("=", 4-m)
	}
	return s
}

// ParsePublicKey 支持 DER SubjectPublicKeyInfo 与裸 PKCS#1 两种形态。
func ParsePublicKey(der []byte) (*rsa.PublicKey, error) {
	if k, err := x509.ParsePKIXPublicKey(der); err == nil {
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
	}
	if rk, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return rk, nil
	}
	return nil, errors.New("无法解析 RSA 公钥（既非 PKIX 也非 PKCS#1）")
}

// NewUUID 生成 RFC4122 v4 UUID 字符串。
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败时退回时间派生（不应发生）。
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// RandomHex 生成 n 字节的十六进制串（traceid 用）。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
