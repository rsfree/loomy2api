package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmptyKeysAllowsAll 未配密钥时一律放行。
func TestEmptyKeysAllowsAll(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if !Verify(req, nil) {
		t.Error("空密钥应放行")
	}
	if !Verify(req, []string{}) {
		t.Error("空切片应放行")
	}
}

// TestBearerAccepted 正确的 Bearer 放行。
func TestBearerAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-ok")
	if !Verify(req, []string{"sk-ok"}) {
		t.Error("正确 Bearer 应放行")
	}
}

// TestXAPIKeyAccepted Anthropic 客户端用 x-api-key，必须支持。
func TestXAPIKeyAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("x-api-key", "sk-ok")
	if !Verify(req, []string{"sk-ok"}) {
		t.Error("x-api-key 应放行")
	}
}

// TestWrongKeyRejected 错误密钥拒绝。
func TestWrongKeyRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer sk-wrong")
	if Verify(req, []string{"sk-ok"}) {
		t.Error("错误密钥不应放行")
	}
}

// TestMissingHeaderRejected 缺头拒绝。
func TestMissingHeaderRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if Verify(req, []string{"sk-ok"}) {
		t.Error("缺鉴权头不应放行")
	}
}

// TestWrongSchemeRejected 方案不对（Basic）拒绝。
func TestWrongSchemeRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic sk-ok")
	if Verify(req, []string{"sk-ok"}) {
		t.Error("Basic 方案不应放行")
	}
}

// TestEmptyTokenRejected 空 token 拒绝。
func TestEmptyTokenRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	if Verify(req, []string{"sk-ok"}) {
		t.Error("空 token 不应放行")
	}
}

// TestMultipleKeys 任一命中即放行。
func TestMultipleKeys(t *testing.T) {
	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+k)
		if !Verify(req, keys) {
			t.Errorf("密钥 %s 应放行", k)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer k4")
	if Verify(req, keys) {
		t.Error("未登记的密钥不应放行")
	}
}

// TestBearerPrefixCaseSensitive 方案前缀大小写敏感（与 HTTP 规范一致）。
func TestBearerPrefixCaseSensitive(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer sk-ok")
	if Verify(req, []string{"sk-ok"}) {
		t.Error("小写 bearer 不应放行")
	}
}
