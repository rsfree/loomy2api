package account

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
)

// TestStringToSignShape 待签名串必须是固定 9 行（末两行空也不能省）。
func TestStringToSignShape(t *testing.T) {
	sts := StringToSign("POST", "/login/account/getPuKey", nil, "", "application/json",
		"Sat, 26 Sep 2026 12:00:00 GMT", "nonce-1", nil)
	lines := strings.Split(sts, "\n")
	if len(lines) != 9 {
		t.Fatalf("期望 9 行，实际 %d 行：%q", len(lines), sts)
	}
	if lines[0] != "POST" {
		t.Errorf("第 1 行应是方法，实际 %q", lines[0])
	}
	if lines[1] != "/login/account/getPuKey" {
		t.Errorf("第 2 行应是路径，实际 %q", lines[1])
	}
	if lines[2] != "" {
		t.Errorf("无 query 时第 3 行应为空，实际 %q", lines[2])
	}
	if lines[3] != "" {
		t.Errorf("无 body 时第 4 行应为空，实际 %q", lines[3])
	}
	if lines[4] != "application/json" {
		t.Errorf("第 5 行应是 content-type，实际 %q", lines[4])
	}
	if lines[7] != "" || lines[8] != "" {
		t.Errorf("无自定义头时末两行应为空，实际 %q / %q", lines[7], lines[8])
	}
}

// TestStringToSignBodyMD5 body 参与 Content-MD5 计算。
func TestStringToSignBodyMD5(t *testing.T) {
	body := `{"a":1}`
	sts := StringToSign("POST", "/x", nil, body, "application/json", "d", "n", nil)
	lines := strings.Split(sts, "\n")
	if lines[3] != ContentMD5(body) {
		t.Errorf("Content-MD5 行不匹配：%q vs %q", lines[3], ContentMD5(body))
	}
	if lines[3] == "" {
		t.Error("有 body 时 Content-MD5 不应为空")
	}
}

// TestEscapePathSegments 路径按段转义，服务端要求的额外转义字符要覆盖。
func TestEscapePathSegments(t *testing.T) {
	cases := map[string]string{
		"/login/account/getPuKey": "/login/account/getPuKey",
		"/a/b c":                  "/a/b%20c",
		"/a/b!c":                  "/a/b%21c",
		"/a/b'c":                  "/a/b%27c",
		"/a/b(c)":                 "/a/b%28c%29",
		"/a/b*c":                  "/a/b%2Ac",
		"/trailing/":              "/trailing",
	}
	for in, want := range cases {
		if got := EscapePath(in); got != want {
			t.Errorf("EscapePath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestEscapeQuerySorted query 按 key 排序，保证签名可复现。
func TestEscapeQuerySorted(t *testing.T) {
	q := map[string]string{"b": "2", "a": "1"}
	if got := EscapeQuery(q); got != "a=1&b=2" {
		t.Errorf("EscapeQuery = %q，期望 a=1&b=2", got)
	}
	if got := EscapeQuery(nil); got != "" {
		t.Errorf("空 query 应为空串，实际 %q", got)
	}
}

// TestCustomHeadersCanonicalized 自定义 x-* 头参与签名（排序 + 尾部换行剥离）。
func TestCustomHeadersCanonicalized(t *testing.T) {
	sts := StringToSign("POST", "/x", nil, "", "application/json", "d", "n",
		map[string]string{"X-B": "2", "X-A": "1", "Other": "ignored"})
	lines := strings.Split(sts, "\n")
	if lines[7] != "x-a;x-b" {
		t.Errorf("SignedHeaders = %q，期望 x-a;x-b", lines[7])
	}
	// 规范化头本身含内嵌换行，故取第 8 行起的所有内容。
	if got := strings.Join(lines[8:], "\n"); got != "x-a:1\nx-b:2" {
		t.Errorf("CanonicalizedHeaders = %q", got)
	}
	if strings.Contains(sts, "ignored") {
		t.Error("非 x-* 头不应参与签名")
	}
}

// TestSignStable 同输入必须得到同签名（HMAC 无随机性）。
func TestSignStable(t *testing.T) {
	a := Sign("secret", "POST", "/x", nil, "", "application/json", "date", "nonce", nil)
	b := Sign("secret", "POST", "/x", nil, "", "application/json", "date", "nonce", nil)
	if a != b {
		t.Fatalf("签名不稳定：%q vs %q", a, b)
	}
	c := Sign("other", "POST", "/x", nil, "", "application/json", "date", "nonce", nil)
	if a == c {
		t.Error("不同 secret 不应得到相同签名")
	}
}

// TestRSAEncryptRoundTrip 加密结果能被对应私钥解回原文。
func TestRSAEncryptRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("序列化公钥失败: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(der)

	cipherB64, err := RSAEncrypt(pubB64, "p@ssw0rd-中文")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	cipher, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		t.Fatalf("密文 base64 解码失败: %v", err)
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, priv, cipher)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plain) != "p@ssw0rd-中文" {
		t.Errorf("解回 %q，期望原文", string(plain))
	}
}

// TestRSAEncryptMissingPadding 客户端拿到的 base64 常缺 padding，必须能容忍。
func TestRSAEncryptMissingPadding(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubB64 := strings.TrimRight(base64.StdEncoding.EncodeToString(der), "=")
	if _, err := RSAEncrypt(pubB64, "x"); err != nil {
		t.Fatalf("缺 padding 的公钥应能解析: %v", err)
	}
}

// TestParsePKCS1PublicKey 裸 PKCS#1 公钥也要支持。
func TestParsePKCS1PublicKey(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	der := x509.MarshalPKCS1PublicKey(&priv.PublicKey)
	if _, err := ParsePublicKey(der); err != nil {
		t.Fatalf("PKCS#1 公钥应能解析: %v", err)
	}
	if _, err := ParsePublicKey([]byte("not a key")); err == nil {
		t.Error("垃圾数据不应解析成功")
	}
}

// TestNewUUIDFormat v4 UUID 的版本位与变体位必须正确。
func TestNewUUIDFormat(t *testing.T) {
	u := NewUUID()
	if len(u) != 36 {
		t.Fatalf("UUID 长度 %d，期望 36：%s", len(u), u)
	}
	if u[14] != '4' {
		t.Errorf("版本位应为 4，实际 %c", u[14])
	}
	switch u[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("变体位应为 8/9/a/b，实际 %c", u[19])
	}
	if NewUUID() == NewUUID() {
		t.Error("两次生成不应相同")
	}
}

// TestNewIdentityDevIDIsWeb devid 必须是固定常量 "web"（自创格式是风控异常信号）。
func TestNewIdentityDevIDIsWeb(t *testing.T) {
	ident := NewIdentity()
	if ident.DevID != "web" {
		t.Errorf("devid = %q，期望 web", ident.DevID)
	}
	if ident.ModelID != "Web" || ident.Version != "1.0.0" {
		t.Errorf("modelid/version = %q/%q", ident.ModelID, ident.Version)
	}
	if ident.UA != ClientUA {
		t.Errorf("ua = %q", ident.UA)
	}
	if !strings.HasPrefix(ident.CampusDev, CampusPrefix) {
		t.Errorf("campus_device_id 前缀不对：%q", ident.CampusDev)
	}
}

// TestRandomHexLength 十六进制串长度是字节数的两倍。
func TestRandomHexLength(t *testing.T) {
	if got := len(RandomHex(16)); got != 32 {
		t.Errorf("RandomHex(16) 长度 %d，期望 32", got)
	}
	if RandomHex(8) == RandomHex(8) {
		t.Error("两次随机不应相同")
	}
}
