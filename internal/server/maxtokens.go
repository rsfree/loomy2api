package server

// minOutputTokens 是出站 max_tokens 的下限：reasoning token 也算在 max_tokens 里，
// 客户端给 32/64 这种小额度时思考会把预算吃光、正文回空且 finish_reason=length
// （2026-10-21 全量实测：20 个对话模型里 10 个这样），看着像「模型坏了」。
// 只能统一抬，不能按模型分档——上游目录里的 reasoning 标记与真实行为不一致。
// shortcut: 非思考模型也会被多给额度，要按模型分档再收紧。
const minOutputTokens = 512

// liftMaxTokens 把过小的输出额度抬到 minOutputTokens；没给、给了够的、非数字都不动。
func liftMaxTokens(payload map[string]any) {
	if v, ok := intOf(payload["max_tokens"]); ok && v > 0 && v < minOutputTokens {
		payload["max_tokens"] = minOutputTokens
	}
}
