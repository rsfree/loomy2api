// Package config 配置加载与持久化。
//
// 优先级：环境变量 > config.json > 内置默认。路径相对 config.json 所在目录解析，
// 这样整个项目目录可以放在任意位置。
//
// 设计对齐 workbuddy2api-panel：单文件 config.json、首次启动自动生成（含随机
// api_key）、面板可在线保存且区分「热生效」与「需重启」字段。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// 上游默认端点（逆向自 Loomy 桌面客户端 0.9.38，见 docs/PROTOCOL.md）。
const (
	DefaultModelBase   = "https://loomyad.xunfei.cn/api/v1"
	DefaultAccountBase = "https://account.xfinfr.com"
	DefaultAccountApp  = "GM3LOOMY"

	// 客户端自带的账号服务签名常量。可被配置覆盖。
	DefaultAccessKeyID     = "2thryby66wxi53sk"
	DefaultAccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// 客户端只校验该头「存在」。
	DefaultLoomyVersion = "0.9.38"

	// 登录态有效期：客户端明确申请 14 天，且没有 refresh token。
	SessionExpireSeconds = 14 * 24 * 3600

	DefaultListen = ":17890"
)

// 默认模型目录（上游 /models 失败时的兜底）。
var fallbackModels = []Model{
	{ID: "imodel/spark-x", Name: "Spark X2.5 (x0.1)", Multiplier: 0.1, ContextLength: 1048576},
	{ID: "imodel/GLM-5.3-Flash", Name: "GLM 5.3 Flash (x0.8)", Multiplier: 0.8, ContextLength: 1048576},
	{ID: "imodel/deepseek-v4-flash-0731", Name: "DeepSeek V4 Flash 0731 (x3.0)", Multiplier: 3.0, ContextLength: 1048576},
	{ID: "imodel/mimo-v2.5", Name: "MiMo V2.5 (x3.3)", Multiplier: 3.3, ContextLength: 1048576},
	{ID: "imodel/MiniMax-M3", Name: "MiniMax M3 (x4.0)", Multiplier: 4.0, ContextLength: 1048576},
	{ID: "imodel/Kimi-k2.6", Name: "Kimi k2.6 (x6.5)", Multiplier: 6.5, ContextLength: 262144},
	{ID: "imodel/qwen-3.8-max", Name: "Qwen 3.8 Max (x12.0)", Multiplier: 12.0, ContextLength: 1000000},
}

// Model 上游模型目录条目。
type Model struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Multiplier     float64  `json:"multiplier,omitempty"`
	ContextLength  int      `json:"context_length,omitempty"`
	MaxOutput      int      `json:"max_output_tokens,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	ReasoningTiers []string `json:"reasoning_efforts,omitempty"`
	// 以下字段来自真实 /models 响应（2026-10-21 抓包核对）：
	//   - 上游**没有** multiplier 字段，倍率与「限时免费」都写在 name 里，
	//     形如 "Spark-X2.6-preview(限时免费)(x0.0)" / "DeepSeek V4.1 Flash(x2.0)"；
	//   - type=chat|image，protocol=openai_chat，默认档 default_reasoning_effort。
	IsFree           bool   `json:"is_free,omitempty"`  // 限时免费（倍率 0 或名称标注）
	Type             string `json:"type,omitempty"`     // chat | image
	Protocol         string `json:"protocol,omitempty"` // openai_chat | …
	IsDefault        bool   `json:"is_default_model,omitempty"`
	DefaultReasoning string `json:"default_reasoning_effort,omitempty"`
}

// IsImage 是否图像生成模型（上游 type=image）。
//
// 这类模型只能走 /v1/images/generations；上游对它们的 /chat/completions
// 一律回 404「该模型暂未开放」，而 404 会触发账号短冷却 —— 客户端把图像
// 模型当对话模型选时，网关不该让上游用 404 来告知我们自己目录里写着的事。
func (m Model) IsImage() bool { return strings.EqualFold(m.Type, "image") }

// Config 全量配置。字段名即 config.json 的键名（独立结构体便于面板表单绑定）。
type Config struct {
	Listen    string   `json:"listen"`
	APIKeys   []string `json:"api_keys"` // 空 = 不鉴权；面板登录也用这把
	AuthDir   string   `json:"auth_dir"`
	StateFile string   `json:"state_file"`
	LogDir    string   `json:"log_dir"`

	Upstream     string `json:"upstream"`
	AccountBase  string `json:"account_base"`
	AccountAppID string `json:"account_appid"`
	AccessKeyID  string `json:"access_key_id"`
	AccessKeySec string `json:"access_key_secret"`
	LoomyVersion string `json:"loomy_version"`
	Proxy        string `json:"proxy"`

	DefaultModel  string `json:"default_model"`
	StripPrefix   bool   `json:"strip_model_prefix"`
	TimeoutSecond int    `json:"timeout_seconds"`
	MaxRetries    int    `json:"max_retries"`
	// 非流式且 max_tokens ≥ 该值时，内部改走流式再聚合：上游对长时间无响应的
	// 非流式请求约 60s 就回 504，流式同模型能跑几分钟。
	KeepaliveMinTokens int `json:"keepalive_min_tokens"`

	CooldownSeconds     int `json:"cooldown_seconds"`
	SoftRateBaseSeconds int `json:"soft_rate_base_seconds"`
	SoftRateMaxSeconds  int `json:"soft_rate_max_seconds"`
	NotFoundCooldownSec int `json:"not_found_cooldown_seconds"`
	BreakerThreshold    int `json:"breaker_threshold"`
	BreakerBaseSeconds  int `json:"breaker_cooldown_seconds"`
	BreakerMaxSeconds   int `json:"breaker_cooldown_max_seconds"`

	Strategy            string `json:"strategy"`
	PickTopN            int    `json:"pick_top_n"`
	MaxInFlight         int    `json:"max_inflight_per_account"`
	StickyTTLSeconds    int    `json:"sticky_ttl_seconds"`
	StickyMaxEntries    int    `json:"sticky_max_entries"`
	SessionRenewDays    int    `json:"session_renew_before_days"`
	QuotaRefreshMinutes int    `json:"quota_refresh_minutes"`
	BalanceFloor        int    `json:"balance_floor"` // 可用积分低于此值停止接单（0=关闭）
	// PreferDaily 选号优先花「今日赠送额度」（按自然日清零），再动永久积分。
	// 上游不会因赠送额度耗尽报错（静默改扣永久积分、照常 200），只能靠排序实现。
	PreferDaily bool `json:"prefer_daily_quota"`
	// StrictModelCheck 请求的模型不在上游目录里时直接 404，不打上游。
	// 必须默认开启：Loomy 对**认不出的模型名**不报错，而是静默回落到默认模型
	// （实测 `no-such-model-xyz` → 200 且响应里 model 变成 DeepSeek-V4.1-Flash，
	// 按默认模型扣分）。客户端会拿到一个"成功"的错答案，且账单对不上。
	StrictModelCheck bool `json:"strict_model_check"`
	// DeadModelTTLDays 上游回 404「模型暂未开放」时，把该模型记多久（天）。
	// 期间 /v1/models 不列它、请求直接 404、不打上游；0 = 用内置默认 30 天。
	DeadModelTTLDays   int    `json:"dead_model_ttl_days"`
	UsageMaxEntries    int    `json:"usage_max_entries"`
	LogRequests        bool   `json:"log_requests"`
	SendClientInfo     bool   `json:"log_client_info"`
	IdentityMode       string `json:"identity_mode"`
	SessionsFromClient bool   `json:"sessions_from_client"`

	path string `json:"-"`
}

// saveMu 配置文件的写盘互斥（包级：Save 会整体替换 Config，实例级锁无从复制）。
var saveMu sync.Mutex

// Defaults 返回一份完整默认配置。
func Defaults() *Config {
	return &Config{
		Listen:    DefaultListen,
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		LogDir:    "./logs",

		Upstream:     DefaultModelBase,
		AccountBase:  DefaultAccountBase,
		AccountAppID: DefaultAccountApp,
		LoomyVersion: DefaultLoomyVersion,

		DefaultModel:  "imodel/deepseek-v4-flash-0731",
		StripPrefix:   true,
		TimeoutSecond: 3600,
		MaxRetries:    2,

		KeepaliveMinTokens: 1500,

		CooldownSeconds:     300,
		SoftRateBaseSeconds: 600,
		SoftRateMaxSeconds:  7200,
		NotFoundCooldownSec: 60,
		BreakerThreshold:    5,
		BreakerBaseSeconds:  60,
		BreakerMaxSeconds:   1800,

		Strategy:            "weighted",
		PickTopN:            5,
		StickyTTLSeconds:    1800,
		StickyMaxEntries:    2000,
		SessionRenewDays:    3,
		QuotaRefreshMinutes: 30,
		BalanceFloor:        0,
		PreferDaily:         true,
		StrictModelCheck:    true,
		DeadModelTTLDays:    30,
		UsageMaxEntries:     20000,
		LogRequests:         true,
		SendClientInfo:      true,
		IdentityMode:        "per_account",
		SessionsFromClient:  true,
	}
}

// Load 读取配置：先取默认值，再叠加文件，最后叠加环境变量。
func Load(path string) (*Config, error) {
	cfg := Defaults()
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cfg.path = abs

	raw, err := os.ReadFile(abs)
	switch {
	case err == nil:
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		// 未知字段不该阻塞启动（用户可能手写了注释键），故先用宽松解码再合并。
		var loose map[string]any
		if jerr := json.Unmarshal(raw, &loose); jerr != nil {
			return nil, fmt.Errorf("config %s 解析失败: %w", abs, jerr)
		}
		if jerr := mergeJSON(cfg, loose); jerr != nil {
			return nil, fmt.Errorf("config %s 字段类型错误: %w", abs, jerr)
		}
	case os.IsNotExist(err):
		// 允许「无配置文件也能起来」：调用方决定是否写默认文件。
	default:
		return nil, fmt.Errorf("读取 config %s: %w", abs, err)
	}

	cfg.applyEnv()
	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", abs, err)
	}
	return cfg, nil
}

// validate 拒绝会静默跑偏的取值：非法 strategy 以前只在选号时悄悄退回
// weighted，非法 listen 要等重启才炸。
func (c *Config) validate() error {
	switch strings.ToLower(c.Strategy) {
	case "weighted", "round_robin", "balance", "lru":
	default:
		return fmt.Errorf("strategy %q 无效（weighted / round_robin / balance / lru）", c.Strategy)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q 无效（形如 :17890）: %w", c.Listen, err)
	}
	return nil
}

// mergeJSON 把磁盘上的键逐个写进结构体（跳过 _ 开头的注释键）。
func mergeJSON(cfg *Config, raw map[string]any) error {
	clean := map[string]any{}
	for k, v := range raw {
		if strings.HasPrefix(k, "_") {
			continue
		}
		clean[k] = v
	}
	buf, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, cfg)
}

var envMap = map[string]string{
	"LOOMY_LISTEN":          "listen",
	"LOOMY_API_KEYS":        "api_keys",
	"LOOMY_AUTH_DIR":        "auth_dir",
	"LOOMY_STATE_FILE":      "state_file",
	"LOOMY_UPSTREAM":        "upstream",
	"LOOMY_ACCOUNT_BASE":    "account_base",
	"LOOMY_AK_ID":           "access_key_id",
	"LOOMY_AK_SECRET":       "access_key_secret",
	"LOOMY_LOOMY_VERSION":   "loomy_version",
	"LOOMY_PROXY":           "proxy",
	"LOOMY_DEFAULT_MODEL":   "default_model",
	"LOOMY_STRATEGY":        "strategy",
	"LOOMY_PICK_TOP_N":      "pick_top_n",
	"LOOMY_MAX_INFLIGHT":    "max_inflight_per_account",
	"LOOMY_STICKY_TTL":      "sticky_ttl_seconds",
	"LOOMY_BALANCE_FLOOR":   "balance_floor",
	"LOOMY_LOG_REQUESTS":    "log_requests",
	"LOOMY_TIMEOUT_SECONDS": "timeout_seconds",
	"LOOMY_PANEL_OFF":       "", // 占位：不接受
}

func (c *Config) applyEnv() {
	buf, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(buf, &m)
	changed := false
	for env, key := range envMap {
		if key == "" {
			continue
		}
		if v, ok := os.LookupEnv(env); ok && v != "" {
			m[key] = coerce(m[key], v)
			changed = true
		}
	}
	if !changed {
		return
	}
	buf, _ = json.Marshal(m)
	_ = json.Unmarshal(buf, c)
}

// coerce 按既有值类型把环境变量字符串转成对应类型。
func coerce(prev any, v string) any {
	switch prev.(type) {
	case bool:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off", "":
			return false
		default:
			return true
		}
	case float64:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return f
		}
		return prev
	case []any:
		parts := strings.Split(v, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	default:
		return v
	}
}

// normalize 修正非法值，保证下游不必到处判空。
func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Upstream == "" {
		c.Upstream = DefaultModelBase
	}
	if c.AccountBase == "" {
		c.AccountBase = DefaultAccountBase
	}
	if c.AccountAppID == "" {
		c.AccountAppID = DefaultAccountApp
	}
	if c.LoomyVersion == "" {
		c.LoomyVersion = DefaultLoomyVersion
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Strategy == "" {
		c.Strategy = "weighted"
	}
	if c.PickTopN <= 0 {
		c.PickTopN = 5
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.TimeoutSecond <= 0 {
		c.TimeoutSecond = 3600
	}
	if c.SoftRateMaxSeconds < c.SoftRateBaseSeconds {
		c.SoftRateMaxSeconds = c.SoftRateBaseSeconds
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 5
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "imodel/deepseek-v4-flash-0731"
	}
}

// AccessKey 返回生效的账号服务 access key（空则用客户端内置常量）。
func (c *Config) AccessKey() (string, string) {
	id := c.AccessKeyID
	if id == "" {
		id = DefaultAccessKeyID
	}
	sec := c.AccessKeySec
	if sec == "" {
		sec = DefaultAccessKeySecret
	}
	return id, sec
}

// Resolve 把配置文件中的相对路径解析成绝对路径（相对 config.json 所在目录）。
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	dir := filepath.Dir(c.path)
	if dir == "" || dir == "." {
		return p
	}
	return filepath.Join(dir, p)
}

// Path 返回配置文件自身的绝对路径。
func (c *Config) Path() string { return c.path }

// Save 深合并写回 config.json（保留未知键、跳过 _ 注释键），返回需重启才生效的键。
func (c *Config) Save(patch map[string]any) ([]string, error) {
	saveMu.Lock()
	defer saveMu.Unlock()

	current := map[string]any{}
	if raw, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(raw, &current)
	}
	if current == nil {
		current = map[string]any{}
	}
	// 合并前的快照：后面判断「重启键是否真的变了」要用它（current 会被 patch 覆盖）。
	before := make(map[string]any, len(current))
	for k, v := range current {
		before[k] = v
	}
	for k, v := range patch {
		if strings.HasPrefix(k, "_") {
			continue
		}
		current[k] = v
	}
	buf, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return nil, err
	}
	// 先按 Config 结构校验，再落盘：类型不对的 patch 曾经写进文件后才发现，
	// 结果配置被写坏、进程重启直接起不来。
	merged := map[string]any{}
	_ = json.Unmarshal(buf, &merged)
	fresh := Defaults()
	fresh.path = c.path
	if err := mergeJSON(fresh, merged); err != nil {
		return nil, err
	}
	if err := fresh.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return nil, err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return nil, err
	}

	// 就地更新内存配置（热生效字段立即生效）。
	fresh.applyEnv()
	fresh.normalize()

	// 只报真正变了的键：面板保存会整表提交，按「提交了就报」会让每次保存都
	// 声称 listen/upstream 要重启，用户无从判断。
	restart := []string{}
	for _, k := range restartKeys {
		v, ok := patch[k]
		if !ok {
			continue
		}
		if old, had := before[k]; had && reflect.DeepEqual(old, v) {
			continue
		}
		restart = append(restart, k)
	}

	// 保留互斥锁指针，其余整体替换。
	path := c.path
	*c = *fresh
	c.path = path
	return restart, nil
}

// restartKeys 改动后需要重启进程才生效的装配期字段。
var restartKeys = []string{"listen", "auth_dir", "state_file", "upstream", "account_base", "log_dir"}

// Public 返回给面板展示的配置（密钥脱敏，绝不回显明文）。
func (c *Config) Public() map[string]any {
	buf, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(buf, &m)
	// api_keys 明文下发（参考面板同款）：面板有「显示」按钮，且保存时整表回传。
	// 看得见这把 key 的人本来就能用它打 /v1 与 /panel，脱敏只会让保存链路变形。
	// access_key_secret 是 Loomy 账号服务签名密钥、参考面板没有对应项，仍然脱敏。
	if sec, _ := m["access_key_secret"].(string); sec != "" {
		m["access_key_secret"] = "…（已设置）"
	}
	m["api_keys_set"] = len(c.APIKeys) > 0
	return m
}

// Ensure 首次启动时生成一份推荐配置，返回 (路径, 新生成的 api_key)。
func Ensure(path string) (string, string, error) {
	if _, err := os.Stat(path); err == nil {
		return path, "", nil
	}
	key, err := RandomAPIKey()
	if err != nil {
		return "", "", err
	}
	cfg := Defaults()
	cfg.APIKeys = []string{key}
	cfg.path = path
	payload := map[string]any{
		"_comment": "由 loomy2api 首启自动生成。listen/auth_dir/state_file/upstream 改动需重启。",
	}
	buf, _ := json.Marshal(cfg)
	_ = json.Unmarshal(buf, &payload)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", err
	}
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", "", err
	}
	return path, key, nil
}

// EnsureAt 首次启动时生成默认配置（不返回 api_key，测试与嵌入方用）。
func EnsureAt(path string) error {
	_, _, err := Ensure(path)
	return err
}

// RandomAPIKey 生成随机网关密钥（crypto/rand）。
func RandomAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-loomy-" + hex.EncodeToString(b), nil
}

// FallbackModels 返回兜底模型目录副本。
func FallbackModels() []Model {
	out := make([]Model, len(fallbackModels))
	copy(out, fallbackModels)
	return out
}
