package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
