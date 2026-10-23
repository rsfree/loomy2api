package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssignBalanced 121 个账号 / 31 个槽位：每个槽位 3 或 4 个，差 ≤ 1。
// 这是「尽量均匀」的可验证口径：不是平均值的错觉，而是逐槽位计数。
func TestAssignBalanced(t *testing.T) {
	const slots = 31
	ids := make([]string, 0, 121)
	for i := 0; i < 121; i++ {
		ids = append(ids, fmt.Sprintf("1380000%04d", i))
	}
	got := Assign(ids, slots)
	if len(got) != len(ids) {
		t.Fatalf("分配数 %d，期望 %d", len(got), len(ids))
	}
	load := make([]int, slots)
	for _, s := range got {
		if s < 0 || s >= slots {
			t.Fatalf("槽位越界: %d", s)
		}
		load[s]++
	}
	lo, hi := load[0], load[0]
	for _, n := range load {
		if n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
	}
	if hi-lo > 1 {
		t.Errorf("负载不均：min=%d max=%d（差应 ≤1）load=%v", lo, hi, load)
	}
	if hi != 4 || lo != 3 {
		t.Errorf("121/31 期望 3~4，得到 %d~%d", lo, hi)
	}
}

// TestAssignDeterministic 同一输入多次调用（含顺序打乱）结果完全一致：
// 分配只由「账号集合 + 槽位数」决定，否则重启就会让所有账号换出口。
func TestAssignDeterministic(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g"}
	first := Assign(ids, 3)
	for i := 0; i < 20; i++ {
		shuffled := append([]string{}, ids...)
		// 简单轮转代替随机：足以改变输入顺序，又不引入随机种子的不确定性。
		shuffled = append(shuffled[i%len(ids):], shuffled[:i%len(ids)]...)
		got := Assign(shuffled, 3)
		for _, id := range ids {
			if got[id] != first[id] {
				t.Fatalf("顺序变化改变了分配：%s %d -> %d", id, first[id], got[id])
			}
		}
	}
}

// TestAssignStableOnAdd 新增一个账号只应影响极少数已有账号。
// 「固定出口」是风控前提，不能因为导入一个号就整体重排。
func TestAssignStableOnAdd(t *testing.T) {
	ids := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		ids = append(ids, fmt.Sprintf("acct-%03d", i))
	}
	before := Assign(ids, 30)
	after := Assign(append(append([]string{}, ids...), "acct-new"), 30)

	moved := 0
	for _, id := range ids {
		if before[id] != after[id] {
			moved++
		}
	}
	if moved > len(ids)/10 {
		t.Errorf("新增 1 个账号挪动了 %d/%d 个已有账号（应 <10%%）", moved, len(ids))
	}
}

// TestResolverExplicitIPs 显式地址列表要排序去重，且 IP() 落到其中一个。
func TestResolverExplicitIPs(t *testing.T) {
	r := New(Config{Mode: ModeLocal, Local: Local{IPs: []string{"127.0.0.2", "127.0.0.1", "127.0.0.2"}}})
	if !r.Enabled() {
		t.Fatalf("未启用：%v", r.Error())
	}
	ips := r.LocalIPs()
	if len(ips) != 2 || ips[0].String() != "127.0.0.1" || ips[1].String() != "127.0.0.2" {
		t.Fatalf("地址未排序去重：%v", ips)
	}
	r.Rebuild([]string{"acct-1"})
	if ip := r.IP("acct-1"); ip == nil || (ip.String() != "127.0.0.1" && ip.String() != "127.0.0.2") {
		t.Fatalf("分配结果不在池内：%v", ip)
	}
	// 未参与 Rebuild 的身份按哈希直接落槽，不能返回 nil（否则静默回落默认出口）。
	if ip := r.IP("acct-unknown"); ip == nil {
		t.Fatal("未登记账号应即时哈希分配，而不是回落默认出口")
	}
}

// TestResolverDisabled 未配置或地址不可用时不启用，且 IP() 返回 nil。
func TestResolverDisabled(t *testing.T) {
	if r := New(Config{}); r.Enabled() {
		t.Error("默认应是 direct")
	} else if ip := r.IP("a"); ip != nil {
		t.Errorf("direct 模式不该绑定源地址：%v", ip)
	}

	r := New(Config{Mode: ModeLocal, Local: Local{IPs: []string{"203.0.113.7"}}})
	if r.Enabled() {
		t.Error("本机不存在的地址不该启用")
	}
	if r.Error() == nil {
		t.Error("应给出地址缺失的原因")
	}
}

// TestLocalSlotCoversAllIPs 均匀性在 Resolver 层也成立：每个 IP 都分到账号，
// 且任意两个 IP 的账号数差 ≤ 1。
func TestLocalSlotCoversAllIPs(t *testing.T) {
	ips := make([]string, 0, 13)
	for i := 1; i <= 13; i++ {
		ips = append(ips, fmt.Sprintf("127.0.0.%d", i))
	}
	r := New(Config{Mode: ModeLocal, Local: Local{IPs: ips}})
	ids := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		ids = append(ids, fmt.Sprintf("u%02d", i))
	}
	r.Rebuild(ids)

	stats := r.Stats()
	if len(stats) != len(ips) {
		t.Fatalf("%d 个 IP 里只有 %d 个分到账号：%v", len(ips), len(stats), stats)
	}
	lo, hi := 0, 0
	first := true
	for _, n := range stats {
		if first || n < lo {
			lo = n
		}
		if first || n > hi {
			hi = n
		}
		first = false
	}
	if hi-lo > 1 {
		t.Errorf("各 IP 账号数差 %d（应 ≤1）：%v", hi-lo, stats)
	}
	if r.Assigned() != len(ids) {
		t.Errorf("Assigned=%d，期望 %d", r.Assigned(), len(ids))
	}
}

// TestClientCachePerIP 每个出口一个 client：不同 IP 不能复用同一个连接池。
func TestClientCachePerIP(t *testing.T) {
	c := NewClientCache(func(ip net.IP) *http.Client { return &http.Client{} })
	a := c.Get(net.ParseIP("127.0.0.1"))
	b := c.Get(net.ParseIP("127.0.0.2"))
	d := c.Get(net.ParseIP("127.0.0.1"))
	if a == b {
		t.Error("不同出口复用了同一个 client")
	}
	if a != d {
		t.Error("同一出口应复用 client")
	}
	if c.Get(nil) == a {
		t.Error("直连 client 不能与绑定出口混用")
	}
}

// TestBindChangesSourceAddr 端到端证据：绑定 127.0.0.2 后，服务端看到的
// RemoteAddr 必须是 127.0.0.2——这是「出口真的换了」的硬断言，
// 而不是「配置里写了就算」。
func TestBindChangesSourceAddr(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		seen = append(seen, host)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	r := New(Config{Mode: ModeLocal, Local: Local{IPs: []string{"127.0.0.2"}}})
	r.Rebuild([]string{"acct-1"})
	cache := NewClientCache(func(ip net.IP) *http.Client {
		if ip == nil {
			return &http.Client{}
		}
		return &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: ip}}).DialContext,
		}}
	})

	for _, identity := range []string{"acct-1", ""} {
		ip := r.IP(identity)
		resp, err := cache.Get(ip).Get(srv.URL)
		if err != nil {
			// macOS 未给 lo0 加 127.0.0.2 别名时 bind 会报 EADDRNOTAVAIL；
			// Linux（生产机）支持整段 127/8，本用例在那里才是有效断言。
			if strings.Contains(err.Error(), "can't assign requested address") {
				t.Skipf("本机不支持 127.0.0.2 源地址（macOS），Linux 上会真实执行: %v", err)
			}
			t.Fatalf("identity=%q 请求失败: %v", identity, err)
		}
		resp.Body.Close()
	}
	if len(seen) != 2 {
		t.Fatalf("服务端只记录了 %d 次请求", len(seen))
	}
	if seen[0] != "127.0.0.2" {
		t.Errorf("绑定出口后源地址是 %s，期望 127.0.0.2", seen[0])
	}
	if seen[1] != "127.0.0.1" {
		t.Errorf("未绑定时应走默认源地址 127.0.0.1，实际 %s", seen[1])
	}
}
