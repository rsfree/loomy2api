package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/pool"
)

// TestPairInvitesEven 偶数号两两配对：每号绑搭档的码，两条互指，谁都不落单。
func TestPairInvitesEven(t *testing.T) {
	cands := []inviteRow{
		{Name: "c", Code: "CCCCCC", Enabled: true, SessionOK: true},
		{Name: "a", Code: "AAAAAA", Enabled: true, SessionOK: true},
		{Name: "d", Code: "DDDDDD", Enabled: true, SessionOK: true},
		{Name: "b", Code: "BBBBBB", Enabled: true, SessionOK: true},
	}
	plan, unpaired := pairInvites(cands, "")
	if len(unpaired) != 0 {
		t.Fatalf("不该有落单：%v", unpaired)
	}
	if len(plan) != 4 {
		t.Fatalf("配对数 %d，期望 4", len(plan))
	}
	// 排序后是 a,b,c,d → 对子 (a,b) 与 (c,d)。
	want := map[string]string{"a": "BBBBBB", "b": "AAAAAA", "c": "DDDDDD", "d": "CCCCCC"}
	for _, as := range plan {
		if got := want[as.Account]; got != as.Code {
			t.Errorf("%s 绑 %s，期望 %s", as.Account, as.Code, got)
		}
	}
}

// TestPairInvitesOddFallback 奇数号：末尾那个绑 fallback，且不允许绑到自己的码。
func TestPairInvitesOddFallback(t *testing.T) {
	cands := []inviteRow{
		{Name: "a", Code: "AAAAAA", Enabled: true, SessionOK: true},
		{Name: "b", Code: "BBBBBB", Enabled: true, SessionOK: true},
		{Name: "c", Code: "CCCCCC", Enabled: true, SessionOK: true},
	}
	plan, unpaired := pairInvites(cands, "MAIN01")
	if len(unpaired) != 0 {
		t.Fatalf("有 fallback 时不该有落单：%v", unpaired)
	}
	var tail *InviteAssignment
	for i := range plan {
		if plan[i].Account == "c" {
			tail = &plan[i]
		}
	}
	if tail == nil || tail.Code != "MAIN01" || tail.Note != "奇数补位" {
		t.Fatalf("末尾号应绑 fallback：%+v", tail)
	}

	// fallback 等于自己的码时必须拒绝（否则白烧一次机会）。
	plan2, unpaired2 := pairInvites(cands, "CCCCCC")
	if len(unpaired2) != 1 || unpaired2[0] != "c" {
		t.Fatalf("fallback 撞自己的码应落单：plan=%v unpaired=%v", plan2, unpaired2)
	}
}

// TestPairInvitesSingleNoFallback 只有一个号且没 fallback：不产生任何绑定。
func TestPairInvitesSingleNoFallback(t *testing.T) {
	plan, unpaired := pairInvites([]inviteRow{{Name: "a", Code: "AAAAAA", Enabled: true, SessionOK: true}}, "")
	if len(plan) != 0 || len(unpaired) != 1 {
		t.Fatalf("单号无 fallback 应原样落单：plan=%v unpaired=%v", plan, unpaired)
	}
}

// TestFilterCandidates 只留下「真能绑」的号：已绑定 / 无自持码 / 登录失效都要排除。
func TestFilterCandidates(t *testing.T) {
	rows := []inviteRow{
		{Name: "ok", Code: "AAAAAA", Enabled: true, SessionOK: true},
		{Name: "bound", Code: "BBBBBB", Enabled: true, SessionOK: true, Applied: "XXXXXX", Bound: true},
		{Name: "nocode", Enabled: true, SessionOK: true},
		{Name: "dead", Code: "DDDDDD", Enabled: true, SessionOK: false},
		{Name: "disabled", Code: "EEEEEE", Enabled: false, SessionOK: true},
		{Name: "err", Code: "FFFFFF", Enabled: true, SessionOK: true, Error: "boom"},
	}
	got := filterCandidates(rows, nil)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("候选集不对：%+v", got)
	}
	// 白名单：只处理指定号。
	if got := filterCandidates(rows, []string{"bound", "ok", "unknown"}); len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("白名单过滤不对：%+v", got)
	}
}

// TestInvitesDryRun 预览：不发 POST、按上游真实状态生成计划。
func TestInvitesDryRun(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/activation") && r.Method == http.MethodPost:
			posts++ // 预览阶段绝不允许出现
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{"activated": true}})
		case strings.HasSuffix(r.URL.Path, "/points/activation"):
			// acc1 已绑过，acc2 未绑 → 计划里只该出现 acc2 相关的一条。
			bound := strings.Contains(r.Header.Get("Authorization"), "sess-1")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{
				"activated": bound, "appliedInvitationCode": map[bool]string{true: "OLD001", false: ""}[bound],
			}})
		case strings.HasSuffix(r.URL.Path, "/invitation-codes"):
			sess := r.Header.Get("Authorization")
			code := "CODE1A"
			if strings.Contains(sess, "sess-2") {
				code = "CODE2B"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{
				"list": []map[string]any{{"inviteCode": code, "usedCount": 0, "maxUses": 1000, "status": "active"}},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	expire := time.Now().Add(7 * 24 * time.Hour).Unix()
	p.Add(&pool.Account{Name: "acc2", Enabled: true, Session: "sess-2", ExpireAt: expire})
	p.Add(&pool.Account{Name: "acc3", Enabled: true, Session: "sess-3", ExpireAt: expire})

	body := `{"mode":"target","target":"MAIN01","confirm":false}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/invites/bind", strings.NewReader(body))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}
	if posts != 0 {
		t.Fatalf("预览阶段发了 %d 个 POST", posts)
	}

	var resp struct {
		DryRun bool               `json:"dry_run"`
		Plan   []InviteAssignment `json:"plan"`
		Sum    struct {
			Bindable  int   `json:"bindable"`
			Bound     int   `json:"bound"`
			Potential int64 `json:"potential"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !resp.DryRun {
		t.Error("应标记 dry_run")
	}
	// acc1 已绑、acc2/acc3 可绑 → 计划两条（各自绑 MAIN01）。
	if len(resp.Plan) != 2 {
		t.Fatalf("计划条数 %d，期望 2：%+v", len(resp.Plan), resp.Plan)
	}
	for _, as := range resp.Plan {
		if as.Code != "MAIN01" {
			t.Errorf("%s 绑了 %s，期望 MAIN01", as.Account, as.Code)
		}
	}
	if resp.Sum.Bindable != 2 || resp.Sum.Bound != 1 {
		t.Errorf("统计不对：%+v", resp.Sum)
	}
	// 每个可绑号换 10000（自己 5000 + 邀请方 5000）。
	if resp.Sum.Potential != 20000 {
		t.Errorf("预期收益 %d，期望 20000", resp.Sum.Potential)
	}
}

// TestInvitesBindConflict 同一时刻只允许一个绑定任务。
func TestInvitesBindConflict(t *testing.T) {
	pl, _ := newTestPanelUpstream(t, "http://127.0.0.1:1")
	job := &inviteJob{running: true, total: 3}
	pl.inviteMu.Lock()
	pl.inviteJob = job
	pl.inviteMu.Unlock()

	if pl.startInviteJob([]InviteAssignment{{Account: "a", Code: "X1"}}, time.Millisecond) {
		t.Fatal("已有任务在跑时不该再启动")
	}
	view := pl.inviteJobView()
	if view["running"] != true {
		t.Fatalf("进度快照不对：%+v", view)
	}
}

// newInviteUpstream 造一个只认邀请码三端点的假上游。
// state 用指针传：测试要在断言里看「有没有真的发过 POST」。
type inviteProbe struct {
	gets  int32 // GET /points/activation 次数
	posts int32 // POST /points/activation 次数
	bound bool  // 是否已绑定
}

func newInviteUpstream(t *testing.T, probe *inviteProbe) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/activation") && r.Method == http.MethodPost:
			atomic.AddInt32(&probe.posts, 1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			probe.bound = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "000000",
				"data": map[string]any{
					"activated": true, "alreadyActivated": false,
					"inviteeReward": 5000, "inviterReward": 5000,
					"currentBalance": 20000, "permanentBalance": 20000,
					"appliedInvitationCode": body["inviteCode"],
					"invitationCodes":       []map[string]any{{"inviteCode": "OWN001", "usedCount": 0, "maxUses": 1000, "status": "active"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/points/activation"):
			atomic.AddInt32(&probe.gets, 1)
			data := map[string]any{"activated": probe.bound}
			if probe.bound {
				data["appliedInvitationCode"] = "OLD999"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": data})
		case strings.HasSuffix(r.URL.Path, "/invitation-codes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{
				"list": []map[string]any{{"inviteCode": "OWN001", "usedCount": 0, "maxUses": 1000, "status": "active"}},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAutoBindInviteBinds 未绑定账号：查一次 → 绑一次 → 账本落 invite_applied。
func TestAutoBindInviteBinds(t *testing.T) {
	probe := &inviteProbe{}
	srv := newInviteUpstream(t, probe)
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	pl.cfg.InviteAuto = true
	pl.cfg.InviteMaxUses = 5

	expire := time.Now().Add(7 * 24 * time.Hour).Unix()
	// 码要从池子里来：一个持码号（src）+ 一个待绑号（new1）。
	src := &pool.Account{Name: "src", Enabled: true, Session: "s-src", ExpireAt: expire, InviteCode: "INV001"}
	p.Add(src)
	acc := &pool.Account{Name: "new1", Enabled: true, Session: "s-new", ExpireAt: expire,
		Identity: account.Identity{CampusDev: "loomy-campus-test"}}
	p.Add(acc)

	pts, err := pl.autoBindInvite(acc)
	if err != nil {
		t.Fatalf("自动绑码失败: %v", err)
	}
	if pts != 5000 {
		t.Errorf("到账 %d，期望 5000", pts)
	}
	if probe.posts != 1 {
		t.Errorf("POST 次数 %d，期望 1", probe.posts)
	}
	if acc.InviteApplied != "INV001" {
		t.Errorf("invite_applied = %q", acc.InviteApplied)
	}
	// 持码方的额度账本要当场 +1（否则下一次选码还会选它）。
	if src.InviteUsed != 1 {
		t.Errorf("src.invite_used = %d，期望 1", src.InviteUsed)
	}
	if acc.InviteReward != 5000 {
		t.Errorf("invite_reward = %d", acc.InviteReward)
	}
	// 绑定响应会带回自持码，落进账号（后续可作为邀请方参与互绑）。
	if acc.InviteCode != "OWN001" {
		t.Errorf("invite_code = %q", acc.InviteCode)
	}
}

// TestAutoBindInviteLocalSkip 本地已记过账：一个上游请求都不发。
func TestAutoBindInviteLocalSkip(t *testing.T) {
	probe := &inviteProbe{}
	srv := newInviteUpstream(t, probe)
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	pl.cfg.InviteAuto = true
	pl.cfg.InviteCode = "CARQ4Y"

	acc := &pool.Account{Name: "done1", Enabled: true, Session: "s", ExpireAt: time.Now().Add(time.Hour).Unix(),
		InviteApplied: "OLD999"}
	p.Add(acc)

	if pts, err := pl.autoBindInvite(acc); err != nil || pts != 0 {
		t.Fatalf("应直接跳过: pts=%d err=%v", pts, err)
	}
	if probe.gets != 0 || probe.posts != 0 {
		t.Errorf("不该打上游：gets=%d posts=%d", probe.gets, probe.posts)
	}
}

// TestAutoBindInviteUpstreamBound 上游已绑定（本地没记）：只回写、不消耗。
func TestAutoBindInviteUpstreamBound(t *testing.T) {
	probe := &inviteProbe{bound: true}
	srv := newInviteUpstream(t, probe)
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	pl.cfg.InviteAuto = true
	pl.cfg.InviteCode = "CARQ4Y"

	acc := &pool.Account{Name: "b1", Enabled: true, Session: "s", ExpireAt: time.Now().Add(time.Hour).Unix()}
	p.Add(acc)

	if pts, err := pl.autoBindInvite(acc); err != nil || pts != 0 {
		t.Fatalf("上游已绑应跳过: pts=%d err=%v", pts, err)
	}
	if probe.posts != 0 {
		t.Errorf("不该发 POST，实际 %d 次", probe.posts)
	}
	if acc.InviteApplied != "OLD999" {
		t.Errorf("应回写实际绑定的码，得到 %q", acc.InviteApplied)
	}
}

// TestAutoBindInviteDisabled 关闭开关或没填码：零动作、零请求。
func TestAutoBindInviteDisabled(t *testing.T) {
	probe := &inviteProbe{}
	srv := newInviteUpstream(t, probe)
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	expire := time.Now().Add(time.Hour).Unix()
	acc := &pool.Account{Name: "x", Enabled: true, Session: "s", ExpireAt: expire}
	p.Add(acc)

	// 开关关：连状态都不查。
	pl.cfg.InviteAuto = false
	if pts, _ := pl.autoBindInvite(acc); pts != 0 || probe.gets != 0 {
		t.Fatalf("关闭时不该动作")
	}
	// 开关开但池里一个可用码都没有（只有自己）：同样不发请求。
	pl.cfg.InviteAuto = true
	p.Get("x").InviteCode = "SELF01"
	if pts, _ := pl.autoBindInvite(acc); pts != 0 {
		t.Fatalf("池内无余量时不该绑")
	}
}

// TestSweepInvites 只扫「未绑 + 有效」的号，已绑的一律不发请求。
func TestSweepInvites(t *testing.T) {
	probe := &inviteProbe{}
	srv := newInviteUpstream(t, probe)
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	pl.cfg.InviteAuto = true
	pl.cfg.InviteCode = "CARQ4Y"

	expire := time.Now().Add(7 * 24 * time.Hour).Unix()
	// 测试脚手架自带的 acc1 也是未绑号，先给它记上账，否则它会被扫进来。
	if a := p.Get("acc1"); a != nil {
		a.InviteApplied = "PRE001"
		a.InviteCode = "POOL01" // 作为码源（否则池内无码可绑）
	}
	p.Add(&pool.Account{Name: "need", Enabled: true, Session: "s1", ExpireAt: expire})
	p.Add(&pool.Account{Name: "done", Enabled: true, Session: "s2", ExpireAt: expire, InviteApplied: "OLD999"})
	p.Add(&pool.Account{Name: "off", Enabled: false, Session: "s3", ExpireAt: expire})

	n, pts := pl.SweepInvites(context.Background())
	if n != 1 || pts != 5000 {
		t.Fatalf("扫到 %d 个号 %d 分，期望 1 个 5000", n, pts)
	}
	if probe.posts != 1 {
		t.Errorf("POST 次数 %d，期望 1（只处理 need）", probe.posts)
	}
}

// TestReserveInviteCodeRespectsMax 选码：用满的不选、优先 used 最少的、
// 全满时返回 false（不能超发）。
func TestReserveInviteCodeRespectsMax(t *testing.T) {
	pl, p := newTestPanelUpstream(t, "http://127.0.0.1:1")
	pl.cfg.InviteMaxUses = 5
	expire := time.Now().Add(time.Hour).Unix()
	p.Add(&pool.Account{Name: "a", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "MAXED1", InviteUsed: 5})
	p.Add(&pool.Account{Name: "b", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "LOW001", InviteUsed: 1})
	p.Add(&pool.Account{Name: "c", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "MID001", InviteUsed: 3})
	p.Add(&pool.Account{Name: "d", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "DEAD01", InviteExhausted: true})

	// 第一个该挑 used 最少的 LOW001。
	got, ok := pl.reserveInviteCode("")
	if !ok || got.Code != "LOW001" {
		t.Fatalf("选到 %+v ok=%v，期望 LOW001", got, ok)
	}
	if p.Get("b").InviteUsed != 2 {
		t.Errorf("预占后 invite_used = %d，期望 2", p.Get("b").InviteUsed)
	}

	// 继续占用直到全满：总容量 = b 剩 4 格 + c 剩 2 格 = 6 格。
	n := 1
	for {
		if _, ok := pl.reserveInviteCode(""); !ok {
			break
		}
		n++
		if n > 20 {
			t.Fatal("超额发放：池子只有 6 格")
		}
	}
	if n != 6 {
		t.Errorf("共发出 %d 格，期望 6（low 余 4 + mid 余 2）", n)
	}
	// 全满之后必须一个都发不出来。
	if _, ok := pl.reserveInviteCode(""); ok {
		t.Error("用满后仍能选到码：会超发")
	}
}

// TestReserveInviteCodeSkipsOwn 不能绑自己的码。
func TestReserveInviteCodeSkipsOwn(t *testing.T) {
	pl, p := newTestPanelUpstream(t, "http://127.0.0.1:1")
	pl.cfg.InviteMaxUses = 5
	expire := time.Now().Add(time.Hour).Unix()
	p.Add(&pool.Account{Name: "self", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "SELF01"})

	if got, ok := pl.reserveInviteCode("SELF01"); ok {
		t.Fatalf("不该选到自己的码：%+v", got)
	}
}

// TestAutoBindInviteSwitchesCode 首个码被上游拒绝 → 标记 exhausted 并换下一个。
func TestAutoBindInviteSwitchesCode(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/points/activation") && r.Method == http.MethodPost:
			atomic.AddInt32(&posts, 1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["inviteCode"] == "BAD001" {
				// 业务错误走 200 + 错误信封，Do 会改判 502。
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": "100001", "desc": "邀请码已达使用上限",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{
				"activated": true, "alreadyActivated": false,
				"inviteeReward": 5000, "currentBalance": 20000,
				"appliedInvitationCode": body["inviteCode"],
			}})
		case strings.HasSuffix(r.URL.Path, "/points/activation"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{"activated": false}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	pl.cfg.InviteAuto = true
	pl.cfg.InviteMaxUses = 5
	expire := time.Now().Add(7 * 24 * time.Hour).Unix()
	// 名字排序让 BAD001 的持有者先被选中。
	bad := &pool.Account{Name: "aaa", Enabled: true, Session: "s", ExpireAt: expire, InviteCode: "BAD001"}
	good := &pool.Account{Name: "zzz", Enabled: true, Session: "s2", ExpireAt: expire, InviteCode: "GOOD01"}
	p.Add(bad)
	p.Add(good)
	acc := &pool.Account{Name: "newbie", Enabled: true, Session: "s3", ExpireAt: expire}
	p.Add(acc)

	pts, err := pl.autoBindInvite(acc)
	if err != nil {
		t.Fatalf("应换码成功而不是报错: %v", err)
	}
	if pts != 5000 {
		t.Errorf("到账 %d，期望 5000", pts)
	}
	if atomic.LoadInt32(&posts) != 2 {
		t.Errorf("POST 次数 %d，期望 2（先 BAD 再 GOOD）", posts)
	}
	if !bad.InviteExhausted {
		t.Error("BAD001 应被标记 exhausted")
	}
	if good.InviteUsed != 1 {
		t.Errorf("GOOD01 应被占用 1 格，实际 %d", good.InviteUsed)
	}
	if acc.InviteApplied != "GOOD01" {
		t.Errorf("invite_applied = %q", acc.InviteApplied)
	}
}
