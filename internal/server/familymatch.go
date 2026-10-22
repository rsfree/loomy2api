package server

import (
	"strconv"
	"strings"

	"github.com/rsfree/loomy2api/internal/config"
)

// 图像端点的「同族回退」：目录里没有请求的模型名时，按家族前缀找同族图像模型。
//
// 相比给某个厂商写死别名，家族归并更通用，也不会把不同厂商的模型串到一起：
//
//	doubao-seedream-4-0-250828 → doubao-seedream-5-lite
//	qwen-image-2.5-preview     → qwen-image-3.0-pro
//
// 精确匹配永远优先（由 matchCatalog 保证）：目录里真有的名字不会被家族回退改写。

// modelFamily 取模型名的「家族键」——第一个版本段之前的部分。
//
//	doubao-seedream-4-0-250828 → doubao-seedream
//	qwen-image-2.5-preview     → qwen-image
//	Hy-Image-3.5-preview       → hy-image
//	qwen3.8-flash              → qwen3.8-flash（首段自带版本，不做家族归并）
//
// 版本段的判定：以数字开头，或 v/V + 数字（v4.1）。分隔符只认 - _ / :，
// 不切 `.`（qwen3.8 这类把版本写进首段的命名，切开反而会误判成 "qwen"）。
func modelFamily(name string) string {
	_, base := splitModelPrefix(strings.TrimSpace(name))
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return ""
	}
	segs := strings.FieldsFunc(base, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == ':'
	})
	cut := len(segs)
	for i, s := range segs {
		if isVersionSeg(s) {
			cut = i
			break
		}
	}
	fam := strings.Join(segs[:cut], "-")
	// 家族键太短会变成「同厂商即同族」，容易在无关模型之间乱跳。
	// 要求至少两段且够长：doubao-seedream / qwen-image / hy-image 通过，
	// 单个 deepseek / qwen / kimi 不通过。
	if len(fam) < 8 || !strings.Contains(fam, "-") {
		return ""
	}
	return fam
}

// isVersionSeg 该段是否是版本号（4 / 3.0 / 250828 / v4.1）。
func isVersionSeg(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
		if s == "" {
			return false
		}
	}
	c := s[0]
	return c >= '0' && c <= '9'
}

// modelVersion 取家族键之后的版本号（doubao-seedream-5-lite → 5，
// qwen-image-3.0-pro → 3.0）；取不到返回 0。
func modelVersion(name string) float64 {
	_, base := splitModelPrefix(strings.TrimSpace(name))
	base = strings.ToLower(base)
	fam := modelFamily(base)
	if fam == "" {
		return 0
	}
	rest := strings.TrimLeft(base[len(fam):], "-_/:")
	end := 0
	for end < len(rest) {
		c := rest[end]
		if c >= '0' && c <= '9' || c == '.' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(rest[:end], 64)
	if err != nil {
		return 0
	}
	return v
}

// familyFallback 在目录里找与请求同族的图像模型，同族多个取版本最高的一个。
// 只考虑 type=image 的条目：这是生图端点的兜底，不该落到对话模型上。
func familyFallback(list []config.Model, requested string) (config.Model, bool) {
	fam := modelFamily(requested)
	if fam == "" {
		return config.Model{}, false
	}
	var hit config.Model
	best := 0.0
	found := false
	for _, m := range list {
		if !m.IsImage() || modelFamily(m.ID) != fam {
			continue
		}
		v := modelVersion(m.ID)
		if !found || v > best {
			hit, best, found = m, v, true
		}
	}
	return hit, found
}
