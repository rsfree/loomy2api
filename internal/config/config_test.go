package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultsSane 默认值必须自洽（冷却封顶 ≥ 基数、必填项非空）。
func TestDefaultsSane(t *testing.T) {
	c := Defaults()
	if c.Listen == "" || c.Upstream == "" || c.AccountBase == "" {
		t.Error("关键默认值为空")
	}
	if c.SoftRateMaxSeconds < c.SoftRateBaseSeconds {
		t.Error("软冷却封顶不能小于基数")
	}
	if c.DefaultModel == "" {
		t.Error("默认模型不能为空")
	}
	if c.AccessKeyID != "" || c.AccessKeySec != "" {
		t.Error("默认不该内置 access key（应由 AccessKey() 回落常量）")
	}
}

// TestLoadMissingFileUsesDefaults 配置文件不存在时用默认值且不报错。
func TestLoadMissingFileUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("缺文件不应报错: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q", cfg.Listen)
	}
}

// TestLoadOverridesDefaults 文件值覆盖默认值。
func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":9999","max_retries":7,"strategy":"round_robin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.MaxRetries != 7 {
		t.Errorf("max_retries = %d", cfg.MaxRetries)
	}
	if cfg.Strategy != "round_robin" {
		t.Errorf("strategy = %q", cfg.Strategy)
	}
	// 未指定项应保留默认值
	if cfg.PickTopN != 5 {
		t.Errorf("pick_top_n = %d，期望默认 5", cfg.PickTopN)
	}
}

// TestLoadIgnoresUnderscoreKeys 下划线注释键不参与解析、也不影响启动。
func TestLoadIgnoresUnderscoreKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{"_comment":"这是注释","_note":{"nested":1},"listen":":7777"}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("注释键不应导致失败: %v", err)
	}
	if cfg.Listen != ":7777" {
		t.Errorf("listen = %q", cfg.Listen)
	}
}

// TestLoadRejectsBrokenJSON 坏 JSON 必须报错（不能静默用默认值）。
func TestLoadRejectsBrokenJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("坏 JSON 应报错")
	}
}

// TestNormalizeFixesIllegalValues 非法值被修正。
func TestNormalizeFixesIllegalValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{"pick_top_n":0,"max_retries":-5,"timeout_seconds":0,
		"soft_rate_base_seconds":900,"soft_rate_max_seconds":100,"breaker_threshold":0}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PickTopN <= 0 {
		t.Error("pick_top_n 应被修正为正数")
	}
	if cfg.MaxRetries < 0 {
		t.Error("max_retries 不应为负")
	}
	if cfg.ImageDeadlineSecond <= 0 {
		t.Errorf("image_deadline_seconds = %d", cfg.ImageDeadlineSecond)
	}
	if cfg.TimeoutSecond <= 0 {
		t.Error("timeout_seconds 应被修正")
	}
	if cfg.SoftRateMaxSeconds < cfg.SoftRateBaseSeconds {
		t.Error("封顶应被抬到不低于基数")
	}
	if cfg.BreakerThreshold <= 0 {
		t.Error("breaker_threshold 应被修正")
	}
}

// TestAccessKeyFallback 空 access key 回落客户端内置常量。
func TestAccessKeyFallback(t *testing.T) {
	c := Defaults()
	id, sec := c.AccessKey()
	if id != DefaultAccessKeyID {
		t.Errorf("id = %q，期望回落常量", id)
	}
	if sec != DefaultAccessKeySecret {
		t.Errorf("secret = %q，期望回落常量", sec)
	}

	c.AccessKeyID = "custom"
	c.AccessKeySec = "secret2"
	id, sec = c.AccessKey()
	if id != "custom" || sec != "secret2" {
		t.Errorf("自定义 key 未生效: %q / %q", id, sec)
	}
}

// TestSaveRoundTrip 保存后重新读取值一致。
func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1111","pick_top_n":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	restart, err := cfg.Save(map[string]any{"pick_top_n": 9, "strategy": "lru"})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(restart) != 0 {
		t.Errorf("这些都是热生效字段，不该要求重启: %v", restart)
	}
	// 内存立即生效
	if cfg.PickTopN != 9 || cfg.Strategy != "lru" {
		t.Errorf("内存配置未热生效: %d / %s", cfg.PickTopN, cfg.Strategy)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PickTopN != 9 || reloaded.Strategy != "lru" {
		t.Errorf("落盘值不对: %d / %s", reloaded.PickTopN, reloaded.Strategy)
	}
	// 未改动的键要保留
	if reloaded.Listen != ":1111" {
		t.Errorf("未改动键丢失: %q", reloaded.Listen)
	}
}

// TestSaveReportsRestartRequired 装配期字段应报告需重启。
func TestSaveReportsRestartRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	restart, err := cfg.Save(map[string]any{"listen": ":8888"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range restart {
		if k == "listen" {
			found = true
		}
	}
	if !found {
		t.Errorf("listen 应列入需重启字段，实际 %v", restart)
	}
}

// TestSavePreservesUnknownKeys 未知键与注释键不能因保存而丢失。
func TestSavePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{"_comment":"保留我","listen":":1111","custom_key":{"a":1}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Save(map[string]any{"pick_top_n": 4}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["_comment"] != "保留我" {
		t.Error("注释键被删除了")
	}
	if m["custom_key"] == nil {
		t.Error("未知键被删除了")
	}
	if m["pick_top_n"] != float64(4) {
		t.Errorf("新值未写入: %v", m["pick_top_n"])
	}
}

// TestPublicKeepsAPIKeysMasksAccountSecret 面板视图口径（对齐参考面板）：
// api_keys 明文下发（面板有「显示」按钮、保存要整表回传），access_key_secret 仍脱敏。
func TestPublicKeepsAPIKeysMasksAccountSecret(t *testing.T) {
	c := Defaults()
	c.APIKeys = []string{"sk-abcdefghijklmnop"}
	c.AccessKeySec = "very-secret"

	pub := c.Public()
	keys := pub["api_keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("api_keys 长度 %d", len(keys))
	}
	if keys[0].(string) != "sk-abcdefghijklmnop" {
		t.Errorf("api_key 应明文下发，实际 %v", keys[0])
	}
	if sec := pub["access_key_secret"].(string); strings.Contains(sec, "very-secret") {
		t.Errorf("access key secret 未脱敏: %v", sec)
	}
	if pub["api_keys_set"] != true {
		t.Errorf("api_keys_set = %v", pub["api_keys_set"])
	}
}

// TestEnsureCreatesConfigWithRandomKey 首次启动生成配置并带随机密钥。
func TestEnsureCreatesConfigWithRandomKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	gotPath, key, err := Ensure(path)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if gotPath != path {
		t.Errorf("路径 = %q", gotPath)
	}
	if !strings.HasPrefix(key, "sk-loomy-") {
		t.Errorf("api_key 前缀不对: %q", key)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("生成的配置无法加载: %v", err)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != key {
		t.Errorf("生成的 api_key 与返回值不一致: %v vs %q", cfg.APIKeys, key)
	}

	// 已存在时不覆盖、不返回新 key
	_, key2, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if key2 != "" {
		t.Error("已存在时不应生成新 key")
	}
}

// TestRandomAPIKeyUnique 随机密钥不应重复。
func TestRandomAPIKeyUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		k, err := RandomAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k] {
			t.Fatalf("生成重复密钥: %s", k)
		}
		seen[k] = true
		if len(k) < 20 {
			t.Errorf("密钥过短: %q", k)
		}
	}
}

// TestResolveRelativePaths 相对路径按配置文件目录解析。
func TestResolveRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_dir":"./auths"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Resolve(cfg.AuthDir)
	if !filepath.IsAbs(got) {
		t.Errorf("应解析成绝对路径，实际 %q", got)
	}
	if !strings.HasSuffix(got, "auths") {
		t.Errorf("路径尾部不对: %q", got)
	}
	// 绝对路径原样返回
	abs := filepath.Join(dir, "x")
	if cfg.Resolve(abs) != abs {
		t.Errorf("绝对路径不应被改写")
	}
}

// TestEnvOverride 环境变量优先级最高。
func TestEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1111","pick_top_n":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOMY_LISTEN", ":2222")
	t.Setenv("LOOMY_PICK_TOP_N", "8")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":2222" {
		t.Errorf("环境变量未覆盖 listen: %q", cfg.Listen)
	}
	if cfg.PickTopN != 8 {
		t.Errorf("环境变量未覆盖 pick_top_n: %d", cfg.PickTopN)
	}
}

// TestExampleConfigFileLoads 仓库里的 config.example.json 必须能被真实加载
// （注释键要被跳过、类型要匹配）——否则用户复制它就会启动失败。
func TestExampleConfigFileLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("找不到 config.example.json: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config.example.json 无法加载: %v", err)
	}
	if cfg.Listen == "" || cfg.Upstream == "" || cfg.AccountBase == "" {
		t.Error("示例配置的关键字段为空")
	}
	if cfg.DefaultModel == "" || cfg.Strategy == "" || cfg.PickTopN <= 0 {
		t.Error("示例配置的模型/策略字段异常")
	}
	// 示例里的占位注释键不应被当成真实配置项
	if cfg.MaxRetries < 0 || cfg.TimeoutSecond <= 0 {
		t.Error("示例配置的数值字段异常")
	}
}

// TestFallbackModels 兜底模型表非空且 id 带 provider 前缀。
func TestFallbackModels(t *testing.T) {
	list := FallbackModels()
	if len(list) == 0 {
		t.Fatal("兜底模型表为空")
	}
	for _, m := range list {
		if !strings.Contains(m.ID, "/") {
			t.Errorf("模型 id 应带 provider 前缀（上游形态 provider/model）: %q", m.ID)
		}
		if m.ID == "" {
			t.Error("模型 id 不能为空")
		}
	}
	// 返回副本：外部改动不应污染内置表
	list[0].ID = "mutated"
	if FallbackModels()[0].ID == "mutated" {
		t.Error("应返回副本而非内部切片")
	}
}

// TestSaveRejectsBadTypeWithoutTouchingFile 类型不对的 patch 必须整体拒绝，
// 且不能把文件写坏（曾出现写坏后连重启都起不来）。
func TestSaveRejectsBadTypeWithoutTouchingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := []byte(`{"listen":":1111","pick_top_n":3}`)
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Save(map[string]any{"pick_top_n": "abc"}); err == nil {
		t.Fatal("类型不对的 patch 应报错")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(orig) {
		t.Errorf("文件被改动: %s", raw)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("文件应仍可加载: %v", err)
	}
}

// TestValidateRejectsBadValues 非法 strategy / listen 必须在加载与保存时都拦住，
// 不能静默退回 weighted 或等重启才炸。
func TestValidateRejectsBadValues(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"strategy":"weight"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Error("非法 strategy 应在加载时报错")
	}

	ok := filepath.Join(dir, "ok.json")
	if err := os.WriteFile(ok, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(ok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Save(map[string]any{"strategy": "weight"}); err == nil {
		t.Error("非法 strategy 应在保存时报错")
	}
	if _, err := cfg.Save(map[string]any{"listen": "17890"}); err == nil {
		t.Error("非法 listen 应在保存时报错")
	}
	if _, err := cfg.Save(map[string]any{"strategy": "round_robin", "listen": "127.0.0.1:18080"}); err != nil {
		t.Errorf("合法值不该报错: %v", err)
	}
}

// TestSaveOnlyReportsChangedRestartKeys 面板保存会整表提交，值没变的重启键
// 不能报成「需重启」，否则每次保存都在说谎。
func TestSaveOnlyReportsChangedRestartKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":1111","upstream":"https://a.example/api","pick_top_n":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// 整表提交：重启键原样带回 + 一个热生效键改动
	restart, err := cfg.Save(map[string]any{
		"listen": ":1111", "upstream": "https://a.example/api", "pick_top_n": 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(restart) != 0 {
		t.Errorf("值未变的重启键不该上报: %v", restart)
	}
	restart, err = cfg.Save(map[string]any{"listen": ":2222", "upstream": "https://a.example/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(restart) != 1 || restart[0] != "listen" {
		t.Errorf("真改了的 listen 应上报，实际 %v", restart)
	}
}

// TestOnboardingDefaults 新手任务自动补领默认开启、间隔 6 小时。
func TestOnboardingDefaults(t *testing.T) {
	c := Defaults()
	if !c.OnboardingAuto {
		t.Error("onboarding_auto 默认应为 true")
	}
	if c.OnboardingIntervalHours != 6 {
		t.Errorf("onboarding_interval_hours = %d，期望 6", c.OnboardingIntervalHours)
	}
}

// TestInviteDefaults 自动绑码默认「开着但没码」。
//
// 这个组合是有意的：开关默认 true 表示「一旦配了码就生效」，而码必须由用户
// 自己填（不能把某个人的邀请码写进开源默认值）。所以默认状态下零动作、零请求。
func TestInviteDefaults(t *testing.T) {
	c := Defaults()
	if !c.InviteAuto {
		t.Error("invite_auto 默认应为 true")
	}
	if c.InviteCode != "" {
		t.Errorf("invite_code 默认应为空，得到 %q", c.InviteCode)
	}
}
