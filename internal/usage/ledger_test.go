package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLedgerRecordAndSummary 写入后能按窗口汇总。
func TestLedgerRecordAndSummary(t *testing.T) {
	l := New("", 100)
	defer l.Close()

	now := time.Now().Unix()
	l.Record(Record{Time: now, Account: "a", Model: "m1", Status: 200,
		Prompt: 10, Completion: 20, Points: 3, LatencyMs: 100})
	l.Record(Record{Time: now, Account: "a", Model: "m2", Status: 500,
		Prompt: 5, Completion: 0, Points: 0, LatencyMs: 50})

	win := l.Summary(map[string]int64{"24h": 86400})["24h"]
	if win.Total != 2 {
		t.Errorf("total = %d，期望 2", win.Total)
	}
	if win.OK != 1 || win.Fail != 1 {
		t.Errorf("ok/fail = %d/%d，期望 1/1", win.OK, win.Fail)
	}
	if win.Prompt != 15 || win.Completion != 20 {
		t.Errorf("token 汇总 = %d/%d，期望 15/20", win.Prompt, win.Completion)
	}
	if win.Points != 3 {
		t.Errorf("points = %d，期望 3", win.Points)
	}
	if win.AvgLatencyMs != 75 {
		t.Errorf("avg latency = %d，期望 75", win.AvgLatencyMs)
	}
	if win.ByModel["m1"] == nil || win.ByModel["m1"].Requests != 1 {
		t.Errorf("按模型分组不对: %v", win.ByModel)
	}
	if win.ByAccount["a"] == nil || win.ByAccount["a"].Requests != 2 {
		t.Errorf("按账号分组不对: %v", win.ByAccount)
	}
}

// TestLedgerWindowExcludesOld 窗口外的记录不计入。
func TestLedgerWindowExcludesOld(t *testing.T) {
	l := New("", 100)
	defer l.Close()

	now := time.Now().Unix()
	l.Record(Record{Time: now - 90000, Model: "old", Status: 200})
	l.Record(Record{Time: now, Model: "new", Status: 200})

	win := l.Summary(map[string]int64{"24h": 86400})["24h"]
	if win.Total != 1 {
		t.Errorf("24h 窗口 total = %d，期望 1", win.Total)
	}
	if win.ByModel["old"] != nil {
		t.Error("窗口外记录不应出现")
	}
}

// TestLedgerRingCap 环形缓冲按上限截断。
func TestLedgerRingCap(t *testing.T) {
	l := New("", 10)
	defer l.Close()
	for i := 0; i < 50; i++ {
		l.Record(Record{Status: 200, Model: "m"})
	}
	recs := l.Recent(1000, RecordFilter{})
	if len(recs) != 10 {
		t.Errorf("记录数 %d，期望压到上限 10", len(recs))
	}
}

// TestLedgerRecentOrderAndFilter 最近记录倒序 + 筛选。
func TestLedgerRecentOrderAndFilter(t *testing.T) {
	l := New("", 100)
	defer l.Close()

	now := time.Now().Unix()
	l.Record(Record{Time: now - 3, Account: "a", Model: "m1", Status: 200})
	l.Record(Record{Time: now - 2, Account: "b", Model: "m2", Status: 500, Error: "boom"})
	l.Record(Record{Time: now - 1, Account: "c", Model: "m1", Status: 200, ClientIP: "1.2.3.4"})

	recs := l.Recent(10, RecordFilter{})
	if len(recs) != 3 {
		t.Fatalf("记录数 %d", len(recs))
	}
	if recs[0].Account != "c" {
		t.Errorf("应是倒序（最新在前），实际 %s", recs[0].Account)
	}

	failed := l.Recent(10, RecordFilter{Result: "fail"})
	if len(failed) != 1 || failed[0].Account != "b" {
		t.Errorf("失败筛选不对: %+v", failed)
	}

	byModel := l.Recent(10, RecordFilter{Model: "m1"})
	if len(byModel) != 2 {
		t.Errorf("按模型筛选 %d 条，期望 2", len(byModel))
	}

	byIP := l.Recent(10, RecordFilter{Keyword: "1.2.3.4"})
	if len(byIP) != 1 {
		t.Errorf("关键词筛选 %d 条，期望 1", len(byIP))
	}
}

// TestLedgerPersistsAcrossRestart 落盘后重启能读回（不因重启清零）。
func TestLedgerPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")

	l := New(path, 100)
	now := time.Now().Unix()
	l.Record(Record{Time: now, Account: "a", Model: "m", Status: 200, Prompt: 7})
	l.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("JSONL 未落盘: %v", err)
	}

	l2 := New(path, 100)
	defer l2.Close()
	win := l2.Summary(map[string]int64{"24h": 86400})["24h"]
	if win.Total != 1 {
		t.Errorf("重启后记录数 %d，期望 1", win.Total)
	}
	if win.Prompt != 7 {
		t.Errorf("重启后 prompt = %d，期望 7", win.Prompt)
	}
}

// TestLedgerHourly 按小时分桶。
func TestLedgerHourly(t *testing.T) {
	l := New("", 100)
	defer l.Close()
	now := time.Now().Unix()
	l.Record(Record{Time: now, Prompt: 3, Completion: 4, Status: 200})

	buckets := l.Hourly(24)
	if len(buckets) != 24 {
		t.Fatalf("桶数 %d，期望 24", len(buckets))
	}
	last := buckets[len(buckets)-1]
	if last["tokens"].(int64) != 7 {
		t.Errorf("最后一桶 tokens = %v，期望 7", last["tokens"])
	}
}

// TestLedgerModels 台账里出现过的模型去重排序。
func TestLedgerModels(t *testing.T) {
	l := New("", 100)
	defer l.Close()
	l.Record(Record{Model: "b", Status: 200})
	l.Record(Record{Model: "a", Status: 200})
	l.Record(Record{Model: "b", Status: 200})

	models := l.Models()
	if len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Errorf("models = %v，期望 [a b]", models)
	}
}

// TestLedgerRange 指定区间汇总。
func TestLedgerRange(t *testing.T) {
	l := New("", 100)
	defer l.Close()
	now := time.Now().Unix()
	l.Record(Record{Time: now - 100, Status: 200})
	l.Record(Record{Time: now - 50, Status: 200})
	l.Record(Record{Time: now, Status: 200})

	win := l.Range(now-60, now)
	if win.Total != 2 {
		t.Errorf("区间内 %d 条，期望 2", win.Total)
	}
}

// TestLedgerPrune 清理过期记录（含文件重写）。
func TestLedgerPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.jsonl")
	l := New(path, 100)
	defer l.Close()

	now := time.Now().Unix()
	l.Record(Record{Time: now - 40*86400, Model: "old", Status: 200})
	l.Record(Record{Time: now - 40*86400, Model: "old2", Status: 200})
	l.Record(Record{Time: now, Model: "new", Status: 200})

	l.Prune(30)

	recs := l.Recent(100, RecordFilter{})
	if len(recs) != 1 || recs[0].Model != "new" {
		t.Errorf("清理后应为 1 条 new，实际 %+v", recs)
	}

	// 文件也应被重写（重启不会复活旧记录）
	l2 := New(path, 100)
	defer l2.Close()
	if got := len(l2.Recent(100, RecordFilter{})); got != 1 {
		t.Errorf("文件重写后应剩 1 条，实际 %d", got)
	}
}
