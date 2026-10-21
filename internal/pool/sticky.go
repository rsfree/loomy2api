// sticky.go 会话粘性：把会话键绑到账号，多轮对话不跳号。
package pool

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Sticky 会话键 → 账号名 的绑定表（TTL 滚动续期 + 容量上限）。
type Sticky struct {
	mu         sync.Mutex
	ttl        float64
	maxEntries int
	m          map[string]stickyEntry
}

type stickyEntry struct {
	Name   string
	Expire float64
}

// NewSticky 构造粘性表。ttl<=0 表示关闭。
func NewSticky(ttl float64, maxEntries int) *Sticky {
	if maxEntries < 0 {
		maxEntries = 0
	}
	return &Sticky{ttl: ttl, maxEntries: maxEntries, m: map[string]stickyEntry{}}
}

// Reconfigure 热更新参数（TTL 与容量）。
func (s *Sticky) Reconfigure(ttl float64, maxEntries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = ttl
	if maxEntries > 0 {
		s.maxEntries = maxEntries
	}
}

// Enabled 是否生效。
func (s *Sticky) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl > 0 && s.maxEntries != 0
}

// Get 取绑定（命中即滚动续期：活跃会话不会中途掉绑定）。
func (s *Sticky) Get(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ttl <= 0 || s.maxEntries == 0 {
		return ""
	}
	n := nowF()
	item, ok := s.m[key]
	if !ok {
		return ""
	}
	if item.Expire <= n {
		delete(s.m, key)
		return ""
	}
	item.Expire = n + s.ttl
	s.m[key] = item
	return item.Name
}

// Bind 写入绑定。
func (s *Sticky) Bind(key, name string) {
	if key == "" || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ttl <= 0 || s.maxEntries == 0 {
		return
	}
	n := nowF()
	if len(s.m) >= s.maxEntries {
		s.pruneLocked(n)
		if len(s.m) >= s.maxEntries {
			// 仍满：丢掉最早到期的一条，保证新会话能绑上。
			oldestKey := ""
			oldest := 0.0
			for k, v := range s.m {
				if oldestKey == "" || v.Expire < oldest {
					oldestKey, oldest = k, v.Expire
				}
			}
			if oldestKey != "" {
				delete(s.m, oldestKey)
			}
		}
	}
	s.m[key] = stickyEntry{Name: name, Expire: n + s.ttl}
}

// Unbind 解绑；传 name 时只在当前绑定确实是它时才解（避免误删新绑定）。
func (s *Sticky) Unbind(key, name string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.m[key]
	if !ok {
		return
	}
	if name != "" && item.Name != name {
		return
	}
	delete(s.m, key)
}

// Prune 清掉过期绑定，返回清理条数。
func (s *Sticky) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.m)
	s.pruneLocked(nowF())
	return before - len(s.m)
}

func (s *Sticky) pruneLocked(n float64) {
	for k, v := range s.m {
		if v.Expire <= n {
			delete(s.m, k)
		}
	}
}

// Snapshot 统计视图。
func (s *Sticky) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"ttl_seconds": s.ttl,
		"entries":     len(s.m),
		"max_entries": s.maxEntries,
	}
}

// SessionKey 从请求体派生会话键。
//
// 优先级：
//  1. 显式会话标识（conversation_id / conversationId / chat_id / chatId）
//  2. 派生键 d-<sha1(最后一条 user)[:16]>
//
// 为何用「最后一条」user 而不是首条：通用客户端每轮重发完整历史，首条消息永远
// 不变 → 键永远相同 → 粘性永久命中同一账号，多账号池退化成单账号（实测请求分布
// 779:13）。取最后一条后多轮对话每轮键都变，粘性只对「同一条消息的重试」生效，
// 这正是它该干的事，轮换交还给选号策略。
func SessionKey(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	for _, f := range []string{"conversation_id", "conversationId", "chat_id", "chatId"} {
		if v, ok := payload[f]; ok {
			if s := toString(v); s != "" {
				return "c-" + s
			}
		}
	}
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return ""
	}
	lastUser := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if toString(m["role"]) != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			lastUser = c
		case []any:
			var parts []string
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t := toString(pm["text"]); t != "" {
						parts = append(parts, t)
					}
				}
			}
			lastUser = strings.Join(parts, " ")
		}
		break
	}
	if lastUser == "" {
		return ""
	}
	if len(lastUser) > 2000 {
		lastUser = lastUser[:2000]
	}
	sum := sha1.Sum([]byte(lastUser))
	return "d-" + hex.EncodeToString(sum[:])[:16]
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strOf(t)
		}
		return strOf(t)
	}
	return ""
}

// touchTime 供测试断言（避免 time 未使用告警）。
var _ = time.Now
