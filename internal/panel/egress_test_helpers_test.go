package panel

import (
	"bytes"
	"log"
	"net/http"
	"testing"

	"github.com/rsfree/loomy2api/internal/egress"
)

// newEgressForPanel 启用单 IP 出口隔离并登记账号（用回环地址，避免依赖本机公网 IP）。
func newEgressForPanel(t *testing.T, names ...string) *egress.Resolver {
	t.Helper()
	r := egress.New(egress.Config{
		Mode:  egress.ModeLocal,
		Local: egress.Local{IPs: []string{"127.0.0.1"}},
	})
	if !r.Enabled() {
		t.Fatalf("出口隔离未启用：%v", r.Error())
	}
	r.Rebuild(names)
	return r
}

// capturePanelLog 收集标准 logger 输出（面板 Logf 走 log.Printf）。
func capturePanelLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// withPathValue 给请求补上 {name} 路径参数（直接调 handler 时 ServeMux 不在链路里）。
func withPathValue(r *http.Request, name string) *http.Request {
	r.SetPathValue("name", name)
	return r
}
