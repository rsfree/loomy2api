package account

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBaseInfoEnvelopeSendsSession baseInfo/thirdInfo 走同款信封：
// param.session 必须存在，签名头必须齐（Authorization/Date/Nonce/Content-MD5）。
func TestSessionQueryCallsEnvelope(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/userinfo/query/baseInfo":
			_, _ = io.WriteString(w, `{"code":"000000","data":{"userid":"123","phone":"156****7692"}}`)
		case "/userinfo/query/thirdInfo":
			// 抓包核对：未绑定第三方时 data 是空数组
			_, _ = io.WriteString(w, `{"code":"000000","desc":"成功","data":[]}`)
		default:
			t.Errorf("未预期路径 %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "APP", "ak", "sk", "")

	data, err := c.BaseInfo("sess-1", Identity{})
	if err != nil {
		t.Fatalf("BaseInfo 失败: %v", err)
	}
	if data["phone"] != "156****7692" {
		t.Errorf("BaseInfo data 不对: %v", data)
	}
	if !strings.HasPrefix(gotAuth, "account ") {
		t.Errorf("Authorization 头应为 account <sig>，实际 %q", gotAuth)
	}
	if gotBody["param"] == nil {
		t.Error("信封缺少 param")
	}

	third, err := c.ThirdInfo("sess-2", Identity{})
	if err != nil {
		t.Fatalf("ThirdInfo 失败: %v", err)
	}
	arr, ok := third.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("ThirdInfo 应返回空数组，实际 %#v", third)
	}
}

// TestSessionQueryErrorEnvelope 业务错误码必须映射成 *Error。
func TestSessionQueryErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"100002","desc":"缺少 token"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "APP", "ak", "sk", "")
	if _, err := c.ThirdInfo("", Identity{}); err == nil {
		t.Fatal("100002 必须报错")
	} else if e, ok := err.(*Error); !ok || e.Code != "100002" {
		t.Errorf("错误类型/码不对: %#v", err)
	}
}
