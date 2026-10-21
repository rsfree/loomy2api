// frontend_test.go 前端接线自检：页面元素与 app.js 的选择器必须对得上。
//
// 这类漂移不会让 Go 编译失败，也不会让任何后端测试变红——只会在浏览器里变成
// 「点按钮没反应」或「某块永远是空的」。所以在这里用最小代价静态对账一次。
package panel

import (
	"regexp"
	"strings"
	"testing"
)

var (
	reIDRef   = regexp.MustCompile(`\$\('#([A-Za-z0-9_-]+)'`)
	reIDDef   = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	reNavView = regexp.MustCompile(`data-view="([a-z]+)"`)
	reSection = regexp.MustCompile(`id="view-([a-z]+)"`)
	reInline  = regexp.MustCompile(`\son(click|submit|change|input)=`)
)

// TestSelectorsResolve app.js 里每个 $('#id') 都能在 index.html 里找到。
func TestSelectorsResolve(t *testing.T) {
	html := string(indexHTML)
	defined := map[string]bool{}
	for _, m := range reIDDef.FindAllStringSubmatch(html, -1) {
		defined[m[1]] = true
	}
	for _, m := range reIDRef.FindAllStringSubmatch(string(appJS), -1) {
		if !defined[m[1]] {
			t.Errorf("app.js 引用了 #%s，但 index.html 里没有这个 id", m[1])
		}
	}
}

// TestNavMatchesViews 每个导航项都要有对应的视图区块，反之亦然。
func TestNavMatchesViews(t *testing.T) {
	html := string(indexHTML)
	nav := map[string]bool{}
	for _, m := range reNavView.FindAllStringSubmatch(html, -1) {
		nav[m[1]] = true
	}
	sec := map[string]bool{}
	for _, m := range reSection.FindAllStringSubmatch(html, -1) {
		sec[m[1]] = true
	}
	for v := range nav {
		if !sec[v] {
			t.Errorf("导航 %q 没有 #view-%s 区块", v, v)
		}
	}
	for v := range sec {
		if !nav[v] {
			t.Errorf("#view-%s 没有对应导航项", v)
		}
	}
}

// TestThemePreloadBeforeStyle 主题预热脚本必须在样式表之前，否则刷新会先闪深色。
func TestThemePreloadBeforeStyle(t *testing.T) {
	html := string(indexHTML)
	i := strings.Index(html, `src="theme.js"`)
	j := strings.Index(html, "<style>")
	if i < 0 || j < 0 {
		t.Fatal("缺少 theme.js 或内联样式块")
	}
	if i > j {
		t.Error("theme.js 必须在 <style> 之前同步加载")
	}
	if !strings.Contains(string(themeJS), "loomy2api.theme") {
		t.Error("theme.js 应读 localStorage['loomy2api.theme']（与 app.js 一致）")
	}
}

// TestNoInlineHandlers CSP 是 script-src 'self'，内联事件处理器会被直接拦掉。
func TestNoInlineHandlers(t *testing.T) {
	if m := reInline.FindString(string(indexHTML)); m != "" {
		t.Errorf("存在内联事件处理器 %q，CSP 会拦", strings.TrimSpace(m))
	}
	if strings.Contains(string(indexHTML), "<script>") {
		t.Error("不应有内联脚本")
	}
}
