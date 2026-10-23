// Package usage 请求台账：内存环形缓冲 + JSONL 落盘，按窗口汇总。
//
// 落盘的原因：网关会频繁重启，纯内存台账会让「用量」页时不时清零。
// 只记元数据（账号 / 模型 / token / 积分 / 耗时 / 状态），不记提示词与响应正文。
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record 一条请求记录。
type Record struct {
	Time       int64  `json:"t"`
	Account    string `json:"account,omitempty"`
	Model      string `json:"model,omitempty"`
	Kind       string `json:"kind,omitempty"` // chat | messages | responses | image | embed | search
	Stream     bool   `json:"stream,omitempty"`
	Status     int    `json:"status"`
	Prompt     int64  `json:"prompt"`
	Completion int64  `json:"completion"`
	Reasoning  int64  `json:"reasoning,omitempty"`
	Points     int64  `json:"points,omitempty"`
	LatencyMs  int64  `json:"latency_ms"`
	TTFBMs     int64  `json:"ttfb_ms,omitempty"`
	ClientIP   string `json:"ip,omitempty"`
	// Egress 本次请求**实际走出去**的本机源 IP（出口隔离的落地证据）。
	// 与 ClientIP 是相反方向：ClientIP 是谁调进来，Egress 是从哪个出口出去。
	// "direct" = 已启用出口隔离但这次请求没绑定账号（未知 session 回落默认出口）。
	Egress    string `json:"egress,omitempty"`
	UserAgent string `json:"ua,omitempty"`
	RequestID string `json:"rid,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Ledger 请求台账。
type Ledger struct {
	mu       sync.Mutex
	records  []Record
	max      int
	filePath string
	f        *os.File
	disabled bool
}

// New 构造台账。filePath 为空表示不落盘；max<=0 用默认 20000。
func New(filePath string, max int) *Ledger {
	if max <= 0 {
		max = 20000
	}
	l := &Ledger{max: max, filePath: filePath}
	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err == nil {
			if f, err := os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				l.f = f
			}
		}
		l.loadTail()
	}
	return l
}

// Close 关闭文件（先把缓冲刷完）。
func (l *Ledger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Sync()
		_ = l.f.Close()
		l.f = nil
	}
}

// loadTail 启动时回读最近记录，避免「重启即清零」。
func (l *Ledger) loadTail() {
	f, err := os.Open(l.filePath)
	if err != nil {
		return
	}
	defer f.Close()

	// 只读最后一段（按大小截取），避免大文件拖慢启动。
	const maxBytes = 8 << 20
	st, err := f.Stat()
	if err != nil {
		return
	}
	start := int64(0)
	if st.Size() > maxBytes {
		start = st.Size() - maxBytes
	}
	if _, err := f.Seek(start, 0); err != nil {
		return
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var tail []Record
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err == nil && r.Time > 0 {
			tail = append(tail, r)
		}
	}
	if len(tail) > l.max {
		tail = tail[len(tail)-l.max:]
	}
	l.records = tail
}

// Record 追加一条记录。
func (l *Ledger) Record(r Record) {
	if r.Time == 0 {
		r.Time = time.Now().Unix()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	if len(l.records) > l.max {
		// 一次性裁掉 10%，避免每条都搬运切片。
		cut := l.max / 10
		if cut < 1 {
			cut = 1
		}
		l.records = l.records[cut:]
	}
	if l.f != nil {
		if buf, err := json.Marshal(r); err == nil {
			_, _ = l.f.Write(append(buf, '\n'))
		}
	}
}

// NetRecord 记一条上游调用（含积分扣费）。
func (l *Ledger) NetRecord(r Record) { l.Record(r) }

// Window 一个统计窗口。
type Window struct {
	From  int64 `json:"from"`
	To    int64 `json:"to"`
	Total int   `json:"total"`
	OK    int   `json:"ok"`
	Fail  int   `json:"fail"`

	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	Points     int64 `json:"points"`

	AvgLatencyMs int64 `json:"avg_latency_ms"`

	ByModel   map[string]*Bucket `json:"by_model"`
	ByAccount map[string]*Bucket `json:"by_account"`
}

// Bucket 分组统计。
type Bucket struct {
	Requests     int   `json:"requests"`
	OK           int   `json:"ok"`
	Fail         int   `json:"fail"`
	Prompt       int64 `json:"prompt_tokens"`
	Completion   int64 `json:"completion_tokens"`
	Points       int64 `json:"points"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
	// Last / LastOK 最近一次记录时间与最近一次成功时间（unix 秒）。面板账号行
	// 「最近成功」直接用它，不必再单独扫一遍台账。
	Last   int64 `json:"last"`
	LastOK int64 `json:"last_ok"`
}

// Summary 按多个窗口汇总。
//
// 只返回窗口内数据：总量随环形缓冲截断，报「历史总量」本身就是错的。
func (l *Ledger) Summary(windows map[string]int64) map[string]*Window {
	l.mu.Lock()
	snapshot := make([]Record, len(l.records))
	copy(snapshot, l.records)
	l.mu.Unlock()

	now := time.Now().Unix()
	out := make(map[string]*Window, len(windows))
	for name, seconds := range windows {
		from := int64(0)
		if seconds > 0 {
			from = now - seconds
		}
		out[name] = aggregate(snapshot, from, now)
	}
	return out
}

// Range 指定时间区间的汇总（面板自定义区间用）。
func (l *Ledger) Range(from, to int64) *Window {
	l.mu.Lock()
	snapshot := make([]Record, len(l.records))
	copy(snapshot, l.records)
	l.mu.Unlock()
	return aggregate(snapshot, from, to)
}

func aggregate(records []Record, from, to int64) *Window {
	w := &Window{
		From: from, To: to,
		ByModel:   map[string]*Bucket{},
		ByAccount: map[string]*Bucket{},
	}
	var latencySum int64
	for _, r := range records {
		if r.Time < from || r.Time > to {
			continue
		}
		w.Total++
		if r.Status >= 200 && r.Status < 300 {
			w.OK++
		} else {
			w.Fail++
		}
		w.Prompt += r.Prompt
		w.Completion += r.Completion
		w.Reasoning += r.Reasoning
		w.Points += r.Points
		latencySum += r.LatencyMs

		bump := func(m map[string]*Bucket, key string) {
			if key == "" {
				key = "-"
			}
			b := m[key]
			if b == nil {
				b = &Bucket{}
				m[key] = b
			}
			b.Requests++
			if r.Status >= 200 && r.Status < 300 {
				b.OK++
			} else {
				b.Fail++
			}
			b.Prompt += r.Prompt
			b.Completion += r.Completion
			b.Points += r.Points
			b.AvgLatencyMs += r.LatencyMs
			if r.Time > b.Last {
				b.Last = r.Time
			}
			if r.Status >= 200 && r.Status < 300 && r.Time > b.LastOK {
				b.LastOK = r.Time
			}
		}
		bump(w.ByModel, r.Model)
		bump(w.ByAccount, r.Account)
	}
	if w.Total > 0 {
		w.AvgLatencyMs = latencySum / int64(w.Total)
	}
	// 分组平均延迟 = 总和 / 次数
	for _, b := range w.ByModel {
		if b.Requests > 0 {
			b.AvgLatencyMs /= int64(b.Requests)
		}
	}
	for _, b := range w.ByAccount {
		if b.Requests > 0 {
			b.AvgLatencyMs /= int64(b.Requests)
		}
	}
	return w
}

// Recent 最近 n 条记录（倒序，面板日志页用）。
func (l *Ledger) Recent(n int, filter RecordFilter) []Record {
	l.mu.Lock()
	snapshot := make([]Record, len(l.records))
	copy(snapshot, l.records)
	l.mu.Unlock()

	out := make([]Record, 0, n)
	for i := len(snapshot) - 1; i >= 0 && len(out) < n; i-- {
		r := snapshot[i]
		if !filter.Match(r) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// RecordFilter 请求记录筛选条件。
type RecordFilter struct {
	From    int64
	To      int64
	Keyword string
	Result  string // ok | fail | ""
	Model   string
	Account string
	// ClientIP / Egress 按子串匹配（大小写不敏感），与 Keyword 同口径：
	// 支持 "154.40.45." 这类前缀写法，排查「某个出口上的全部请求」很方便。
	ClientIP string
	Egress   string
}

// Match 是否命中筛选。
func (f RecordFilter) Match(r Record) bool {
	if f.From > 0 && r.Time < f.From {
		return false
	}
	if f.To > 0 && r.Time > f.To {
		return false
	}
	switch f.Result {
	case "ok":
		if r.Status < 200 || r.Status >= 300 {
			return false
		}
	case "fail":
		if r.Status >= 200 && r.Status < 300 {
			return false
		}
	}
	if f.Model != "" && r.Model != f.Model {
		return false
	}
	if f.Account != "" && r.Account != f.Account {
		return false
	}
	if f.ClientIP != "" && !strings.Contains(strings.ToLower(r.ClientIP), strings.ToLower(f.ClientIP)) {
		return false
	}
	if f.Egress != "" && !strings.Contains(strings.ToLower(r.Egress), strings.ToLower(f.Egress)) {
		return false
	}
	if f.Keyword != "" {
		hay := strings.ToLower(strings.Join([]string{
			r.Model, r.Account, r.RequestID, r.ClientIP, r.Egress, r.UserAgent, r.Error,
			fmt.Sprintf("%d", r.Status),
		}, " "))
		for _, word := range strings.Fields(strings.ToLower(f.Keyword)) {
			if !strings.Contains(hay, word) {
				return false
			}
		}
	}
	return true
}

// Hourly 最近 n 小时的分桶（趋势图用）。
func (l *Ledger) Hourly(hours int) []map[string]any {
	if hours <= 0 {
		hours = 24
	}
	l.mu.Lock()
	snapshot := make([]Record, len(l.records))
	copy(snapshot, l.records)
	l.mu.Unlock()

	now := time.Now().Truncate(time.Hour)
	buckets := make([]map[string]any, hours)
	type agg struct {
		requests   int
		prompt     int64
		completion int64
		points     int64
	}
	data := make([]agg, hours)
	for i := 0; i < hours; i++ {
		buckets[i] = map[string]any{
			"hour": now.Add(time.Duration(i-hours+1) * time.Hour).Format("01-02 15:00"),
		}
	}
	start := now.Add(time.Duration(-hours+1) * time.Hour).Unix()
	for _, r := range snapshot {
		if r.Time < start {
			continue
		}
		idx := int((r.Time - start) / 3600)
		if idx < 0 || idx >= hours {
			continue
		}
		data[idx].requests++
		data[idx].prompt += r.Prompt
		data[idx].completion += r.Completion
		data[idx].points += r.Points
	}
	for i := range buckets {
		buckets[i]["requests"] = data[i].requests
		buckets[i]["prompt_tokens"] = data[i].prompt
		buckets[i]["completion_tokens"] = data[i].completion
		buckets[i]["tokens"] = data[i].prompt + data[i].completion
		buckets[i]["points"] = data[i].points
	}
	return buckets
}

// HourlyRange 指定区间 [from,to]（unix 秒）的小时桶序列，面板用量图用。
//
// 与 Hourly 的区别：Hourly 永远是「现在往回 N 小时」的滚动窗口；这里按显式
// 区间对齐——「今天」视图的柱子必须从本地 00:00 开始，而不是服务器滚动尾巴。
// 区间超过 720 小时时自动加宽桶（按天聚合），保证桶数有界、标签不挤爆。
func (l *Ledger) HourlyRange(from, to int64) []map[string]any {
	l.mu.Lock()
	snapshot := make([]Record, len(l.records))
	copy(snapshot, l.records)
	l.mu.Unlock()

	if to <= 0 {
		to = time.Now().Unix()
	}
	if from <= 0 {
		// 「全部历史」：从最早一条记录起算（records 按写入序即时间序）。
		if len(snapshot) > 0 {
			from = snapshot[0].Time
		} else {
			from = to - 86400
		}
	}
	if from > to {
		from, to = to, from
	}
	span := to - from
	width := int64(3600)
	if span/width > 720 {
		// 加宽到「天」桶：桶数超过 30 天时整图已经不可读，按天是最后的体面。
		width = 86400
	}

	base := time.Unix(from, 0).In(time.Local)
	if width == 3600 {
		base = time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), 0, 0, 0, time.Local)
	} else {
		base = time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, time.Local)
	}
	n := int((to-base.Unix())/width) + 1
	if n < 1 {
		n = 1
	}
	buckets := make([]map[string]any, n)
	type agg struct {
		requests   int
		prompt     int64
		completion int64
		points     int64
	}
	data := make([]agg, n)
	for _, r := range snapshot {
		if r.Time < base.Unix() || r.Time > to {
			continue
		}
		idx := int((r.Time - base.Unix()) / width)
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		data[idx].requests++
		data[idx].prompt += r.Prompt
		data[idx].completion += r.Completion
		data[idx].points += r.Points
	}
	format := "01-02 15:00"
	if width > 3600 {
		format = "01-02"
	}
	for i := range buckets {
		ts := base.Add(time.Duration(i) * time.Duration(width) * time.Second)
		buckets[i] = map[string]any{
			"hour":              ts.Format(format),
			"requests":          data[i].requests,
			"prompt_tokens":     data[i].prompt,
			"completion_tokens": data[i].completion,
			"tokens":            data[i].prompt + data[i].completion,
			"points":            data[i].points,
		}
	}
	return buckets
}

// Models 台账里出现过的模型名（去重排序）。
func (l *Ledger) Models() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range l.records {
		if r.Model != "" {
			seen[r.Model] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Prune 清理超过保留天数的 JSONL（启动与周期性调用）。
func (l *Ledger) Prune(retentionDays int) {
	if l.filePath == "" || retentionDays <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()

	l.mu.Lock()
	kept := l.records[:0]
	for _, r := range l.records {
		if r.Time >= cutoff {
			kept = append(kept, r)
		}
	}
	l.records = kept
	f := l.f
	l.mu.Unlock()

	if f == nil {
		return
	}
	// 重写文件：先读全量、过滤、原子替换。
	raw, err := os.ReadFile(l.filePath)
	if err != nil {
		return
	}
	tmp := l.filePath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	w := bufio.NewWriter(out)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.Time < cutoff {
			continue
		}
		_, _ = w.WriteString(string(sc.Bytes()) + "\n")
	}
	_ = w.Flush()
	_ = out.Close()
	l.mu.Lock()
	if l.f != nil {
		_ = l.f.Close()
	}
	if err := os.Rename(tmp, l.filePath); err == nil {
		if nf, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			l.f = nf
		}
	}
	l.mu.Unlock()
}
