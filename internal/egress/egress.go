// Package egress 账号 → 本机出口 IP 的确定性分配。
//
// 为什么在网关侧算：源地址是内核在 connect 时选定的，只能由发起方指定。
// 上游（讯飞）按源 IP 做风控，把 121 个账号全压在一个出口上，等于让它们
// 共用一份风险额度；分散到本机多个公网 IP 才是有效隔离。
//
// 设计对齐 workbuddy2api 的 egress.local：身份（账号名）经 sha256 落到槽位，
// 槽位对应一个本机地址；本包只做**纯策略**（地址发现 + 哈希 + 负载上限），
// 不建连接、不持有 http.Client——拨号在 upstream / account 侧（见 client.go）。
//
// 与参考实现的一处差异（用户明确要求「尽量均匀」）：纯 hash % n 在
// 121 个账号 / 31 个 IP 上会出现某些 IP 背 8 个、某些背 1 个的偏斜。
// 这里加了一层「容量上限 + 线性探测」：容量 = ceil(账号数/IP 数)，超出容量的
// 账号沿槽位顺序找下一个还有空位的 IP。分配结果仍然只由 (账号集合, IP 列表)
// 决定——同样的输入永远同样的输出，重启不漂移。
package egress

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
)

// 出口模式。
const (
	// ModeDirect 直连：不绑定源地址（内核按默认路由选），未配置时的行为。
	ModeDirect = "direct"
	// ModeLocal 绑定本机多个出口 IP，按账号 hash 分配。
	ModeLocal = "local"
)

// Local 本机多出口 IP 配置。
type Local struct {
	// IPs 显式指定的源地址；空 = 自动发现本机网卡地址。
	// 上游按源 IP 风控时，显式列举（并先探测可达）比自动发现更可控：
	// 自动发现会把「连不出公网」或「已被上游拉黑」的地址也纳进来。
	IPs []string `json:"ips,omitempty"`
	// Interfaces 只在这些网卡上发现地址（空 = 全部网卡）。
	Interfaces []string `json:"interfaces,omitempty"`
	// IncludeIPv6 是否把 IPv6 纳入候选。默认 false：上游对 v6 出口的风控
	// 策略与 v4 不同，混用会让「同账号同出口」的假设失效。
	IncludeIPv6 bool `json:"include_ipv6,omitempty"`
}

// Config 出口策略配置（config.json 的 egress 段）。
type Config struct {
	Mode  string `json:"mode,omitempty"`
	Local Local  `json:"local,omitempty"`
}

// Resolver 出口解析器。并发安全。
type Resolver struct {
	cfg Config

	// ips 启动时冻结的地址快照，已排序：接口枚举顺序在部分平台不稳定，
	// 不排序会让同一账号在重启后换出口，直接破坏「固定出口」的承诺。
	ips []net.IP
	err error

	mu   sync.RWMutex
	slot map[string]int // 账号身份 → 槽位；Rebuild 时整体替换
}

// New 构造解析器。local 模式的地址发现在此处完成一次。
func New(cfg Config) *Resolver {
	if cfg.Mode == "" {
		cfg.Mode = ModeDirect
	}
	r := &Resolver{cfg: cfg, slot: map[string]int{}}
	if cfg.Mode == ModeLocal {
		r.ips, r.err = resolveLocalIPs(cfg.Local)
	}
	return r
}

// Mode 生效模式。
func (r *Resolver) Mode() string {
	if r == nil {
		return ModeDirect
	}
	return r.cfg.Mode
}

// Enabled 是否启用出口隔离。
func (r *Resolver) Enabled() bool {
	return r != nil && r.cfg.Mode == ModeLocal && len(r.ips) > 0
}

// LocalIPs 返回候选出口地址（副本）。
func (r *Resolver) LocalIPs() []net.IP {
	if r == nil {
		return nil
	}
	out := make([]net.IP, len(r.ips))
	copy(out, r.ips)
	return out
}

// Error 地址发现失败的原因（Enabled=false 时用于启动告警）。
func (r *Resolver) Error() error {
	if r == nil {
		return nil
	}
	return r.err
}

// Describe 启动日志用的一句话描述（不含敏感信息）。
func (r *Resolver) Describe() string {
	if r == nil || r.cfg.Mode == "" || r.cfg.Mode == ModeDirect {
		return "direct"
	}
	if r.err != nil {
		return fmt.Sprintf("%s[disabled: %v]", r.cfg.Mode, r.err)
	}
	return fmt.Sprintf("%s[%d ip]", r.cfg.Mode, len(r.ips))
}

// Rebuild 按当前账号集合重算「账号 → 槽位」。
//
// 账号增删时调用（pool 的 OnChange 钩子）；账号集合不变则结果不变。
// 空集合或未启用时清空映射（IP 选择回落默认路由）。
func (r *Resolver) Rebuild(identities []string) {
	if r == nil || !r.Enabled() {
		return
	}
	assign := Assign(identities, len(r.ips))
	r.mu.Lock()
	r.slot = assign
	r.mu.Unlock()
}

// Slot 取账号当前槽位。未参与 Rebuild（例如刚导入、钩子还没跑）时按
// 纯哈希即时计算，保证「有 IP 可用」而不是直接回落默认路由。
func (r *Resolver) Slot(identity string) int {
	if r == nil || !r.Enabled() {
		return -1
	}
	r.mu.RLock()
	s, ok := r.slot[identity]
	r.mu.RUnlock()
	if ok {
		return s
	}
	return int(hash32(identity) % uint32(len(r.ips)))
}

// IP 取账号应绑定的源地址；未启用时为 nil（= 不绑定）。
func (r *Resolver) IP(identity string) net.IP {
	if !r.Enabled() || identity == "" {
		return nil
	}
	s := r.Slot(identity)
	if s < 0 || s >= len(r.ips) {
		return nil
	}
	return r.ips[s]
}

// IPString 同 IP，但直接给日志/面板用的文本；未启用时空串。
func (r *Resolver) IPString(identity string) string {
	if ip := r.IP(identity); ip != nil {
		return ip.String()
	}
	return ""
}

// Stats 各 IP 上分配的账号数（面板展示「是否均匀」用）。
func (r *Resolver) Stats() map[string]int {
	out := map[string]int{}
	if r == nil || !r.Enabled() {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.slot {
		if s >= 0 && s < len(r.ips) {
			out[r.ips[s].String()]++
		}
	}
	return out
}

// Assigned 已参与分配的账号数（与 len(Stats 求和) 一致）。
func (r *Resolver) Assigned() int {
	if r == nil || !r.Enabled() {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.slot)
}

// Assign 把 identities 分配到 slotCount 个槽位。
//
// 规则（确定性）：
//  1. 每个身份先算 32 位哈希，槽位偏好 = hash % slotCount；
//  2. 按 (哈希, 身份) 升序处理，容量上限 cap = ceil(账号数 / 槽位数)；
//  3. 偏好槽位已满则沿 (slot+1) mod n 找下一个未满槽位。
//
// 于是任意两个槽位的账号数差 ≤ 1，且同一份 (账号集合, 槽位数) 永远得到
// 同一结果。增删账号时容量可能 ±1，会挪动少量账号——这是「固定出口」
// 与「尽量均匀」之间的取舍，已知并接受。
func Assign(identities []string, slotCount int) map[string]int {
	out := make(map[string]int, len(identities))
	if slotCount <= 0 || len(identities) == 0 {
		return out
	}
	type item struct {
		id   string
		hash uint32
	}
	items := make([]item, 0, len(identities))
	for _, id := range identities {
		if strings.TrimSpace(id) == "" {
			continue
		}
		items = append(items, item{id: id, hash: hash32(id)})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].hash != items[j].hash {
			return items[i].hash < items[j].hash
		}
		return items[i].id < items[j].id
	})

	// 软上限取 floor：留出 (N mod n) 个名额给「兜底」分配。
	// 若用 ceil，贪心会让部分槽位先被填满、另一些永远等不到人（实测 121/31
	// 会出现 2/3/4 混杂）；floor + 全局最小负载兜底才能保证差 ≤1。
	capPer := len(items) / slotCount
	if capPer < 1 {
		capPer = 1
	}
	load := make([]int, slotCount)
	for _, it := range items {
		start := int(it.hash % uint32(slotCount))
		if load[start] < capPer {
			out[it.id] = start
			load[start]++
			continue
		}
		// 偏好槽位已满：取当前负载最小的槽位（并列时取离偏好最近的顺时针位），
		// 保证最终每个槽位都是 floor 或 floor+1。
		best, bestLoad := -1, int(^uint(0)>>1)
		for i := 0; i < slotCount; i++ {
			s := (start + i) % slotCount
			if load[s] < bestLoad {
				best, bestLoad = s, load[s]
			}
		}
		out[it.id] = best
		load[best]++
	}
	return out
}

func hash32(s string) uint32 {
	sum := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint32(sum[:4])
}

// virtualPrefixes 默认排除的虚拟网卡前缀：这些地址不是出公网的源地址，
// 纳进来会让账号被分到一个连不出去的槽位。
var virtualPrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "vmnet", "vboxnet",
	"utun", "tun", "tap", "wg", "zt", "tailscale", "cni", "flannel", "kube",
}

// resolveLocalIPs 枚举候选出口地址。
//
// 显式配置时**只保留本机真实存在的地址**并给出缺失告警——把不存在的地址
// 留在池里，会在拨号时变成 EADDRNOTAVAIL（每次请求都失败），不如启动就发现。
func resolveLocalIPs(cfg Local) ([]net.IP, error) {
	local := localIPv4Set()
	if len(cfg.IPs) > 0 {
		out := make([]net.IP, 0, len(cfg.IPs))
		var missing []string
		for _, s := range cfg.IPs {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			ip := net.ParseIP(s)
			if ip == nil {
				return nil, fmt.Errorf("egress: 非法 IP %q", s)
			}
			// 回环地址放行：127/8 整段都由本机路由，测试与本机联调要用，
			// 但它们不会出现在网卡地址列表里（只有 127.0.0.1 在）。
			if _, ok := local[ip.String()]; !ok && !ip.IsLoopback() {
				missing = append(missing, s)
				continue
			}
			out = append(out, ip)
		}
		if len(missing) > 0 {
			// 不直接判失败：部分地址暂时不存在（换机器、IP 被回收）不该让整个
			// 出口隔离失效，剩余地址仍能分担；错误只用于启动告警。
			if len(out) == 0 {
				return nil, fmt.Errorf("egress: 配置的地址都不在本机: %s", strings.Join(missing, ","))
			}
			return sortDedup(out), fmt.Errorf("egress: %d 个配置地址不在本机: %s",
				len(missing), strings.Join(missing, ","))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("egress: 没有可用本机地址")
		}
		return sortDedup(out), nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("egress: 列网卡失败: %w", err)
	}
	only := map[string]bool{}
	for _, n := range cfg.Interfaces {
		only[strings.TrimSpace(n)] = true
	}
	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(only) > 0 && !only[iface.Name] {
			continue
		}
		if excludedInterface(iface.Name) {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, perr := net.ParseCIDR(addr.String())
			if perr != nil || ip == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
				continue
			}
			if ip.To4() == nil {
				if !cfg.IncludeIPv6 {
					continue
				}
				ips = append(ips, ip)
				continue
			}
			ips = append(ips, ip.To4())
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("egress: 未发现可用本机地址")
	}
	return sortDedup(ips), nil
}

// localIPv4Set 本机全部地址（含虚拟网卡），用于校验显式配置。
func localIPv4Set() map[string]net.IP {
	out := map[string]net.IP{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, perr := net.ParseCIDR(addr.String())
			if perr == nil && ip != nil && ip.To4() != nil {
				out[ip.String()] = ip.To4()
			}
		}
	}
	return out
}

func excludedInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func sortDedup(in []net.IP) []net.IP {
	seen := map[string]bool{}
	out := make([]net.IP, 0, len(in))
	for _, ip := range in {
		if ip == nil {
			continue
		}
		k := ip.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ip)
	}
	// 按字符串排序：与 workbuddy2api 保持一致，同时让 IPv4 顺序稳定。
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
