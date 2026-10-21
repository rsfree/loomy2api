// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架 + 内联样式（设计令牌与组件全部在这一处，与蓝本 workbuddy2api-panel 同构）
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//   - theme.js    首屏主题预热（必须在样式表之前同步加载，否则刷新会先闪深色）
//
// 安全头对「页面与全部 /panel/api/* 响应」统一生效：CSP 限制脚本只来自本服务、
// 禁止被 iframe 嵌套（防点击劫持）、禁 MIME 嗅探、不外泄 Referer。
package panel

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

//go:embed theme.js
var themeJS []byte

//go:embed favicon.svg
var faviconSVG []byte

// csp 内容安全策略（严格版，无需 unsafe-inline 脚本）：
//   - default-src 'none'   默认全禁，逐个开口
//   - script-src 'self'    只跑同源脚本（app.js）
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是取舍：页面有少量 style="width: xx%" 属性（进度条），
//     允许内联样式不会导致脚本执行；仍禁外部样式域。
//   - connect-src 'self'   前端 fetch 只能打本服务
//   - img-src 'self' data: 图标 / 内联图
//   - frame-ancestors 'none' 禁嵌套
//   - base-uri 'none'      禁注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}

func (p *Panel) themeScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(themeJS)
}

// favicon 内嵌 SVG 图标：没有它浏览器每次开面板都会请求 /favicon.ico 拿 404，
// 控制台里多一条红字，排查真错误时反而碍事。
func (p *Panel) favicon(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconSVG)
}
