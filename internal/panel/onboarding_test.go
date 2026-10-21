package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/upstream"
)

// TestOnboardingMatrix 任务中心矩阵：顺序固定、点数按本地表、不可查询的号不发请求。
func TestOnboardingMatrix(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/onboarding/tasks") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "000000",
			"data": map[string]any{
				"tasks": map[string]bool{"first_message": true, "pick_skill": true, "share_soul": false},
			},
		})
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	// 两个不该打上游的号：一个已禁用，一个登录态失效（ExpireAt 已过）。
	p.Add(&pool.Account{Name: "acc2", Enabled: false, Session: "s2", ExpireAt: 1})
	p.Add(&pool.Account{Name: "acc3", Enabled: true, Session: "s3", ExpireAt: 1})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/onboarding", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
	}

	var resp struct {
		Tasks    []onboardingTaskMeta `json:"tasks"`
		Total    int64                `json:"total"`
		Accounts []struct {
			Name   string          `json:"name"`
			Done   int             `json:"done"`
			Earned int64           `json:"earned"`
			Tasks  map[string]bool `json:"tasks"`
			Error  string          `json:"error"`
		} `json:"accounts"`
		Summary struct {
			Queryable int   `json:"queryable"`
			TasksDone int   `json:"tasks_done"`
			Earned    int64 `json:"earned"`
			Claimable int64 `json:"claimable"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 顺序固定：面板靠它对齐列，不能随 map 遍历抖动。
	if len(resp.Tasks) != len(upstream.OnboardingOrder) {
		t.Fatalf("任务数 %d，期望 %d", len(resp.Tasks), len(upstream.OnboardingOrder))
	}
	var sum int64
	for i, meta := range resp.Tasks {
		if meta.Key != upstream.OnboardingOrder[i] {
			t.Errorf("第 %d 项 = %q，期望 %q", i, meta.Key, upstream.OnboardingOrder[i])
		}
		if meta.Title == "" || meta.Short == "" {
			t.Errorf("任务 %s 缺少展示文案", meta.Key)
		}
		sum += meta.Points
	}
	if sum != upstream.OnboardingTotal || resp.Total != upstream.OnboardingTotal {
		t.Errorf("任务总积分 %d / total %d，期望 %d", sum, resp.Total, upstream.OnboardingTotal)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("上游只该被查 1 次（仅 acc1），实际 %d 次", got)
	}

	if len(resp.Accounts) != 3 {
		t.Fatalf("账号行 %d，期望 3", len(resp.Accounts))
	}
	acc1 := resp.Accounts[0]
	if acc1.Done != 2 || acc1.Earned != 1500 {
		t.Errorf("acc1 done=%d earned=%d，期望 2 / 1500", acc1.Done, acc1.Earned)
	}
	if !acc1.Tasks["first_message"] || acc1.Tasks["share_soul"] {
		t.Error("acc1 任务状态未按上游响应归一")
	}
	if resp.Accounts[1].Error == "" || !strings.Contains(resp.Accounts[1].Error, "禁用") {
		t.Errorf("禁用账号应显形错误，实际 %q", resp.Accounts[1].Error)
	}
	if resp.Accounts[2].Error == "" || !strings.Contains(resp.Accounts[2].Error, "登录态") {
		t.Errorf("失效账号应显形错误，实际 %q", resp.Accounts[2].Error)
	}

	if resp.Summary.Queryable != 1 || resp.Summary.TasksDone != 2 || resp.Summary.Earned != 1500 {
		t.Errorf("汇总错误：%+v", resp.Summary)
	}
	if resp.Summary.Claimable != upstream.OnboardingTotal-1500 {
		t.Errorf("待领积分 %d，期望 %d", resp.Summary.Claimable, upstream.OnboardingTotal-1500)
	}
}
