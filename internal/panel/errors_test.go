// errors_test.go 错误汇总：同一 (账号, 模型, 状态) 合并计数，成功请求不进表。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yuanjie-ai/loomy2api/internal/usage"
)

func TestErrorSummaryGroups(t *testing.T) {
	pl, _ := newTestPanel(t)
	now := time.Now().Unix()
	rec := func(acc, model string, status int, errText string) {
		pl.ledger.Record(usage.Record{
			Time: now, Account: acc, Model: model, Kind: "chat",
			Status: status, Error: errText,
		})
	}
	rec("a", "m1", 502, "登录已失效")
	rec("a", "m1", 502, "登录已失效")
	rec("a", "m1", 429, "rate limit")
	rec("b", "m1", 502, "登录已失效")
	rec("a", "m1", 200, "") // 成功不进汇总

	req := httptest.NewRequest(http.MethodGet, "/panel/api/error_summary", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)

	var out struct {
		Errors []ErrStat `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if len(out.Errors) != 3 {
		t.Fatalf("应有 3 类（a/502、a/429、b/502），实际 %d: %+v", len(out.Errors), out.Errors)
	}
	// 次数降序：a-m1-502 有 2 次，排第一
	top := out.Errors[0]
	if top.Account != "a" || top.Model != "m1" || top.Status != 502 || top.Count != 2 {
		t.Errorf("首行应是 a/m1/502 ×2，实际 %+v", top)
	}
	if top.Sample != "登录已失效" {
		t.Errorf("sample = %q", top.Sample)
	}
	if top.Last == "" {
		t.Error("last 不应为空")
	}
}
