package server

import "testing"

// 思考 token 算在 max_tokens 里：客户端给 32 时正文回空（实测 2026-10-21），
// 出站必须抬到下限；给够的、没给的、非数字的一律不动。
func TestLiftMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"小额度抬到下限", 32, minOutputTokens},
		{"64 也抬", 64, minOutputTokens},
		{"给够了不动", 4096, 4096},
		{"等于下限不动", 512, minOutputTokens},
		{"没给就不发明", nil, nil},
		{"0 当没给", 0, 0},
		{"字符串数字也认", "32", minOutputTokens},
		{"非数字不动", "abc", "abc"},
	}
	for _, c := range cases {
		payload := map[string]any{}
		if c.in != nil {
			payload["max_tokens"] = c.in
		}
		liftMaxTokens(payload)
		if got := payload["max_tokens"]; got != c.want {
			t.Errorf("%s: 出站额度 = %v，期望 %v", c.name, got, c.want)
		}
	}
}
