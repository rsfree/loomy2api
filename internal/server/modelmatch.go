package server

import (
	"strings"

	"github.com/rsfree/loomy2api/internal/config"
)

// modelProviderPrefix 是上游 provider 命名空间前缀。
// 目录可能给带前缀的 id（编译期兜底表），也可能给裸 id（线上 /models 现状），
// 匹配时两边都按「剥掉前缀再比」处理。
const modelProviderPrefix = "imodel/"

// splitModelPrefix 拆出可选 provider 前缀，大小写不敏感。
// 返回原样的前缀（保持客户端写法）与裸 id。
func splitModelPrefix(model string) (prefix, base string) {
	if len(model) >= len(modelProviderPrefix) &&
		strings.EqualFold(model[:len(modelProviderPrefix)], modelProviderPrefix) {
		return model[:len(modelProviderPrefix)], model[len(modelProviderPrefix):]
	}
	return "", model
}

// isModelIDWord 判断字符是否会与模型名粘连（ASCII 字母/数字）。
//
// 前缀匹配要求边界是分隔符：`deepseek-v4.1-flash-xxx` 的 `-` 是合法边界，
// 而 `spark-xylophone` 里的 `y` 不是——否则一个拼错的名字会被悄悄路由到
// 另一个模型上，比直接 404 更难排查。
func isModelIDWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// matchCatalog 按「精确优先 → 最长前缀」把请求里的模型名映射到目录条目。
//
// 上游对认不出的模型名不报错，而是静默用默认模型执行并照常扣分，所以别名
// 必须由网关自己收敛：客户端写 `deepseek-v4.1-flash-xxx`（带日期/版本后缀，
// 各家 SDK 的常见习惯）要能明确落到 `DeepSeek-V4.1-Flash`，而不是赌上游容错。
//
// 匹配规则（均忽略大小写、忽略 imodel/ 前缀）：
//  1. 精确命中目录 id → 用它；
//  2. 否则在目录里找「请求名的最长前缀」，且前缀后一个字符必须是分隔符
//     （非字母数字，如 `-` `.` `:` `@` `/`）；
//  3. 都没命中返回 false，由调用方决定补前缀还是 404。
func matchCatalog(list []config.Model, requested string) (config.Model, bool) {
	_, base := splitModelPrefix(strings.TrimSpace(requested))
	if base == "" {
		return config.Model{}, false
	}

	// 1) 精确匹配（大小写不敏感）。
	for _, m := range list {
		_, id := splitModelPrefix(m.ID)
		if id != "" && strings.EqualFold(id, base) {
			return m, true
		}
	}

	// 2) 最长前缀匹配。
	best := -1
	var hit config.Model
	for _, m := range list {
		_, id := splitModelPrefix(m.ID)
		if id == "" || len(id) > len(base) {
			continue
		}
		if !strings.EqualFold(base[:len(id)], id) {
			continue
		}
		// 前缀后面还有内容时，必须是分隔符边界。
		if len(id) < len(base) && isModelIDWord(base[len(id)]) {
			continue
		}
		if len(id) > best {
			best, hit = len(id), m
		}
	}
	if best >= 0 {
		return hit, true
	}
	return config.Model{}, false
}

// suggestModels 给 404 文案补充候选：目录里以请求名为前缀的模型（最多 n 个）。
// 例如请求 `deepseek-v4` 时提示 `DeepSeek-V4.1-Flash`。
func suggestModels(list []config.Model, requested string, n int) []string {
	_, base := splitModelPrefix(strings.TrimSpace(requested))
	if base == "" || n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, m := range list {
		_, id := splitModelPrefix(m.ID)
		if id == "" || strings.EqualFold(id, base) {
			continue
		}
		if len(id) > len(base) && strings.EqualFold(id[:len(base)], base) {
			out = append(out, m.ID)
			if len(out) == n {
				break
			}
		}
	}
	return out
}
