// ring.go 固定容量的日志环形缓冲（并发安全）。
//
// 与蓝本 workbuddy2api-panel 同款：main 把 log 包输出经 MultiWriter 镜像进来，
// /panel/api/logs 读快照。每行入环时按行首特征归类频道，面板按频道筛选——
// 对话流量大时，运维动作不被请求行冲掉。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// tsPrefixRe log 包默认 flags 产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// 日志频道。
const (
	ChChat = "chat" // 每请求一行表格日志（`| #001 | …`）
	ChTask = "task" // 面板运维动作（登录 / 刷新额度 / 解冻 / 配置保存）
	ChSys  = "sys"  // 其余系统日志
)

// LogEntry 单条日志。行首的 log 包时间戳已剥离，时刻取写入时的 TS。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 面板运维动作的行首标识（本仓 Logf 的口径）。
var taskPrefixes = []string{
	"新增账号", "账号 ", "全量", "配置已保存", "已刷新", "重新登录",
}

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") {
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 环形日志缓冲。
type Ring struct {
	mu  sync.Mutex
	buf []LogEntry
	max int
}

// NewRing 构造缓冲。max<=0 用默认 500。
func NewRing(max int) *Ring {
	if max <= 0 {
		max = 500
	}
	return &Ring{max: max}
}

// Write 实现 io.Writer：main 用 MultiWriter 把 log 包输出镜像进来。
// 每行可能带 `2026/10/20 20:00:00 ` 前缀，剥掉后按行归类入环。
func (r *Ring) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "") // 面板自己再补 HH:MM:SS
		r.Add(text)
	}
	return len(b), nil
}

// Add 写入一条日志（时刻取当前时间）。
func (r *Ring) Add(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, LogEntry{TS: time.Now(), Ch: classifyLine(text), Text: text})
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

// Snapshot 返回副本（面板读取用，不持锁序列化）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.buf))
	copy(out, r.buf)
	return out
}
