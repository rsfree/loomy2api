// account.go 讯飞账号服务（login / userinfo）客户端与登录流程。
package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rsfree/loomy2api/internal/egress"
)

// SessionExpireSeconds 客户端固定申请 14 天登录态。
const SessionExpireSeconds = 14 * 24 * 3600

// 客户端固定身份常量（全部平台一致，自创格式反而显眼）。
const (
	WebModelID    = "Web"
	ClientVersion = "1.0.0"
	ClientUA      = "Loomy|Desktop|Electron|macOS"
	DeviceID      = "web"
	CampusPrefix  = "loomy-campus-"
)

// Error 账号服务业务错误。
type Error struct {
	Path      string
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s failed: %s (code=%s)", e.Path, e.Message, e.Code)
	}
	return fmt.Sprintf("%s failed: %s", e.Path, e.Message)
}

// Identity 一个账号的设备身份。
type Identity struct {
	DevID     string `json:"devid"`
	UA        string `json:"ua"`
	ModelID   string `json:"modelid"`
	Version   string `json:"version"`
	CampusDev string `json:"campus_device_id,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// NewIdentity 生成一份账号身份。
//
// devid 必须是固定常量 "web"：官方全平台客户端都发这个值，自创格式（如
// web-<hex>）在全部真实流量里都是异类。设备区分由 promotions 侧的
// campus_device_id 承担（客户端每台机器一个），这里保持 per-account。
func NewIdentity() Identity {
	return Identity{
		DevID:     DeviceID,
		UA:        ClientUA,
		ModelID:   WebModelID,
		Version:   ClientVersion,
		CampusDev: CampusPrefix + NewUUID(),
		CreatedAt: time.Now().Unix(),
	}
}

func (i Identity) orDefault() Identity {
	if i.DevID == "" {
		return NewIdentity()
	}
	return i
}

// Client 账号服务客户端。
type Client struct {
	BaseURL  string
	AppID    string
	AKID     string
	AKSecret string
	Proxy    string
	HTTP     *http.Client

	// Egress 出口隔离：登录 / 额度等账号域请求也按账号绑定源 IP。
	// 与模型网关用同一份分配，避免「同一账号在两条链路上出口不同」。
	Egress  *egress.Resolver
	clients *egress.ClientCache
}

// NewClient 构造账号服务客户端。
func NewClient(base, appID, akID, akSecret, proxy string) *Client {
	c := &Client{
		BaseURL:  strings.TrimRight(base, "/"),
		AppID:    appID,
		AKID:     akID,
		AKSecret: akSecret,
		Proxy:    proxy,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
	c.clients = egress.NewClientCache(func(ip net.IP) *http.Client {
		return &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
					LocalAddr: &net.TCPAddr{IP: ip},
				}).DialContext,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	})
	return c
}

// httpFor 取该账号应使用的 client；未启用出口隔离时用默认 client。
func (c *Client) httpFor(identity string) *http.Client {
	if c == nil || c.Egress == nil || !c.Egress.Enabled() || identity == "" {
		return c.HTTP
	}
	ip := c.Egress.IP(identity)
	if ip == nil {
		return c.HTTP
	}
	return c.clients.Get(ip)
}

// envelope 账号服务统一请求信封。
func (c *Client) envelope(ident Identity, param map[string]any) map[string]any {
	body := map[string]any{
		"base": map[string]string{
			"appid":   c.AppID,
			"modelid": ident.ModelID,
			"version": ident.Version,
			"devid":   ident.DevID,
			"ua":      ident.UA,
			"traceid": RandomHex(16), // 客户端每个请求都重新生成
		},
	}
	if param != nil {
		body["param"] = param
	}
	return body
}

// Call 发送一次签名请求，返回解析后的 JSON。
// Call 发一次签名请求（不绑定出口；账号域调用请用 callAs 传账号名）。
func (c *Client) Call(path string, body map[string]any) (map[string]any, error) {
	return c.callAs("", path, body)
}

// callAs 按账号绑定出口发请求。identity 为空 = 默认出口。
func (c *Client) callAs(identity, path string, body map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	payload := string(raw)
	headers := BuildHeaders(c.AKID, c.AKSecret, "POST", path, payload, nil)

	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpFor(identity).Do(req)
	if err != nil {
		return nil, &Error{Path: path, Code: "NETWORK", Message: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &Error{
			Path:      path,
			Code:      fmt.Sprintf("HTTP_%d", resp.StatusCode),
			Message:   trunc(string(data), 200),
			Retryable: resp.StatusCode >= 500,
		}
	}
	code := strOf(out["code"], out["errorCode"])
	if code != "" && code != "000000" {
		msg := strOf(out["desc"], out["message"])
		if msg == "" {
			msg = code
		}
		return out, &Error{Path: path, Code: code, Message: msg}
	}
	if resp.StatusCode >= 400 {
		msg := strOf(out["message"])
		if msg == "" {
			msg = trunc(string(data), 200)
		}
		return out, &Error{
			Path: path, Code: fmt.Sprintf("HTTP_%d", resp.StatusCode),
			Message: msg, Retryable: resp.StatusCode >= 500,
		}
	}
	return out, nil
}

// PublicKey 取 RSA 公钥与 rcode（无副作用，适合做连通性探测）。
func (c *Client) PublicKey(ident Identity) (pubkey, rcode string, err error) {
	out, err := c.Call("/login/account/getPuKey", c.envelope(ident.orDefault(), nil))
	if err != nil {
		return "", "", err
	}
	data := mapOf(out["data"])
	pubkey = strOf(data["pukey"])
	rcode = strOf(data["rcode"])
	if pubkey == "" {
		return "", "", &Error{Path: "/login/account/getPuKey", Code: "NO_PUKEY", Message: "服务端未返回公钥"}
	}
	return pubkey, rcode, nil
}

// LoginResult 登录成功后的凭证。
type LoginResult struct {
	Session string
	UserID  string
	Phone   string
}

// LoginByPassword 手机号 + 密码登录（全自动，无需验证码）。
func (c *Client) LoginByPassword(loginID, password string, ident Identity) (*LoginResult, error) {
	return c.LoginByPasswordAs(loginID, loginID, password, ident)
}

// LoginByPasswordAs 同 LoginByPassword，但显式指定**出口身份**（账号名）。
//
// 账号名与 loginid 不一定相同（导入的卡密常有独立备注名），而出口分配是按账号名
// 算的：这里若用 loginid，登录会落到另一个 IP，与后续生成流量不一致——风控看到
// 的就是「同一个号刚换了网络」。
func (c *Client) LoginByPasswordAs(name, loginID, password string, ident Identity) (*LoginResult, error) {
	ident = ident.orDefault()
	// getPuKey 在同一出口取：公钥与登录请求若来自不同 IP，风控看到的是
	// 一次「刚换过网络」的登录，等于自找验证码。
	pkOut, err := c.callAs(name, "/login/account/getPuKey", c.envelope(ident, nil))
	if err != nil {
		return nil, err
	}
	pkData := mapOf(pkOut["data"])
	pubkey, rcode := strOf(pkData["pukey"]), strOf(pkData["rcode"])
	if pubkey == "" {
		return nil, &Error{Path: "/login/account/getPuKey", Code: "NO_PUKEY", Message: "服务端未返回公钥"}
	}
	enc, err := RSAEncrypt(pubkey, password)
	if err != nil {
		return nil, err
	}
	out, err := c.callAs(name, "/login/account/byPwd", c.envelope(ident, map[string]any{
		"loginid":  loginID,
		"password": enc,
		"rcode":    rcode,
		"type":     1,
		"expire":   SessionExpireSeconds,
	}))
	if err != nil {
		return nil, err
	}
	return extractSession(out)
}

// SendSmsCode 发送短信验证码。
func (c *Client) SendSmsCode(phone string, ident Identity) (map[string]any, error) {
	return c.callAs(phone, "/login/phone/sendMsgCode", c.envelope(ident.orDefault(), map[string]any{
		"ccode": "86", "phone": phone, "expire": 300,
	}))
}

// LoginBySms 短信验证码登录。
func (c *Client) LoginBySms(phone, code, msgID string, ident Identity) (*LoginResult, error) {
	out, err := c.callAs(phone, "/login/phone/checkCode", c.envelope(ident.orDefault(), map[string]any{
		"ccode": "86", "phone": phone, "mcode": code, "msgid": msgID,
		"expire": SessionExpireSeconds,
	}))
	if err != nil {
		return nil, err
	}
	return extractSession(out)
}

// BaseInfo 查询账号基本信息（用于验证登录态是否还有效）。
func (c *Client) BaseInfo(session string, ident Identity) (map[string]any, error) {
	out, err := c.Call("/userinfo/query/baseInfo", c.envelope(ident.orDefault(),
		map[string]any{"session": session}))
	if err != nil {
		return nil, err
	}
	return mapOf(out["data"]), nil
}

// ThirdInfo 查询第三方绑定信息（抓包核对：客户端登录成功后与 baseInfo 同批调用，
// 未绑定时 data 为空数组）。
func (c *Client) ThirdInfo(session string, ident Identity) (any, error) {
	out, err := c.Call("/userinfo/query/thirdInfo", c.envelope(ident.orDefault(),
		map[string]any{"session": session}))
	if err != nil {
		return nil, err
	}
	return out["data"], nil
}

// Logout 注销登录态。
func (c *Client) Logout(session string, ident Identity) error {
	_, err := c.Call("/login/account/logout", c.envelope(ident.orDefault(),
		map[string]any{"session": session}))
	return err
}

func extractSession(payload map[string]any) (*LoginResult, error) {
	node := mapOf(payload["data"])
	if len(node) == 0 {
		node = payload
	}
	session := strOf(node["session"])
	if session == "" {
		return nil, &Error{Code: "NO_SESSION", Message: "登录成功但未返回 session"}
	}
	return &LoginResult{
		Session: session,
		UserID:  strOf(node["userid"], node["userId"]),
		Phone:   strOf(node["phone"], node["loginid"]),
	}, nil
}

// ---------------------------------------------------------------- helpers

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func strOf(vals ...any) string {
	for _, v := range vals {
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case float64:
			return fmt.Sprintf("%.0f", t)
		case json.Number:
			return t.String()
		}
	}
	return ""
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
