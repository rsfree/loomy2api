// invites.go 邀请码：账号 × 邀请码矩阵 + 「两两互绑」/「统一绑到某码」批量执行。
//
// 为什么单开一页，而不是塞进账号池的一行按钮：
//
//	邀请码是**不可逆的一次性资源**——上游一个账号只能绑一次别人的码
//	（绑完 activated 永久为真，再提交只回 alreadyActivated 且不再给分）。
//	而奖励是双边的：被邀方 +5000、邀请方 +5000。于是「A 绑 B 的码 +
//	B 绑 A 的码」这个动作能让一双号各拿 10000，本质上是把 N 次一次性
//	凭证换成 2N 份奖励。选错码、重复执行一次，就永久烧掉该号的 5000。
//
//	这种操作必须：① 预览与实际执行同一份计划；② 逐号可见结果；
//	③ 执行前现场复查 activation（本地账本只做预筛，不信缓存做判据）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/upstream"
)

const (
	// inviteConcurrency 矩阵查询并发：每号 2 个 GET，121 号 = 242 次。
	// 参照 credit_history 的教训——并发太低会把整页拖成空白，4 路跑 121 号
	// 实测 40s+；8 路约 12s，且不会给上游造成瞬时压力。
	inviteConcurrency = 8
	inviteRowTimeout  = 15 * time.Second
	inviteTotalBudget = 90 * time.Second

	// InviteRewardPerBind 一次绑定给被邀方的积分（上游 inviteeReward，实测 5000）。
	// 双方各 5000，所以 pair 模式下每号预期 10000（自己绑一次 + 自己被绑一次）。
	InviteRewardPerBind int64 = 5000

	inviteDefaultDelayMS = 1200
)

// inviteRow 单号邀请码视图（GET /panel/api/invites 的一行）。
type inviteRow struct {
	Name      string                `json:"name"`
	Remark    string                `json:"remark,omitempty"`
	Enabled   bool                  `json:"enabled"`
	SessionOK bool                  `json:"session_ok"`
	Egress    string                `json:"egress,omitempty"`
	Codes     []upstream.InviteCode `json:"codes,omitempty"`
	Code      string                `json:"code,omitempty"`    // 主码（codes[0]，展示与配对都用它）
	Applied   string                `json:"applied,omitempty"` // 已绑别人的码
	Bound     bool                  `json:"bound"`             // 上游 activated
	Available int64                 `json:"available,omitempty"`
	// Used / Max 走本地口径（InviteUsed / invite_max_uses），不是上游那个
	// 恒为 1000 的 maxUses：面板要显示的是「这个码还能再邀几个人」。
	Used      int    `json:"used"`
	Max       int    `json:"max"`
	Exhausted bool   `json:"exhausted,omitempty"`
	Error     string `json:"error,omitempty"`
}

// canBind 这个号还能不能花掉它的一次性绑定机会。
func (r inviteRow) canBind() bool {
	return r.Enabled && r.SessionOK && !r.Bound && r.Code != "" && r.Error == ""
}

// InviteAssignment 一条「谁绑谁的码」。
type InviteAssignment struct {
	Account string `json:"account"`        // 执行绑定的号
	Code    string `json:"code"`           // 要绑的码
	From    string `json:"from,omitempty"` // 码的持有者（pair 模式下是搭档，target 模式下是目标码主人）
	Note    string `json:"note,omitempty"`
}

// InviteResult 一条执行结果。
type InviteResult struct {
	Account       string `json:"account"`
	Code          string `json:"code"`
	OK            bool   `json:"ok"`
	Skipped       bool   `json:"skipped,omitempty"`
	Reason        string `json:"reason,omitempty"`
	InviteeReward int64  `json:"invitee_reward,omitempty"`
	Balance       int64  `json:"balance,omitempty"`
}

// collectInvites 拉全池邀请码矩阵。
//
// 每号两个 GET（activation + invitation-codes），并发 8 路、单号 15s、
// 总预算 90s。查询失败的号带 error 显形，不与「已绑定」或「无码」混为一谈：
// 三者对「还能不能绑」的结论完全不同（未知 ≠ 已用掉）。
func (p *Panel) collectInvites(ctx context.Context) ([]inviteRow, map[string]any) {
	ctx, cancel := context.WithTimeout(ctx, inviteTotalBudget)
	defer cancel()

	accounts := p.pool.All()
	rows := make([]inviteRow, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, inviteConcurrency)

	for i, acc := range accounts {
		rows[i] = inviteRow{
			Name: acc.Name, Remark: acc.Remark, Enabled: acc.Enabled,
			SessionOK: acc.SessionValid(), Code: acc.InviteCode,
			Applied: acc.InviteApplied, Bound: acc.InviteApplied != "",
			Egress: p.egressOf(acc.Name),
			Used:   acc.InviteUsed, Max: p.inviteMaxUses(),
			Exhausted: acc.InviteExhausted,
		}
		if acc.Available != nil {
			rows[i].Available = *acc.Available
		}
		if !acc.Enabled {
			rows[i].Error = "已禁用，跳过查询"
			continue
		}
		if !acc.SessionValid() {
			rows[i].Error = "登录态已失效，需要重新登录"
			continue
		}
		wg.Add(1)
		go func(i int, acc *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, ccancel := context.WithTimeout(ctx, inviteRowTimeout)
			defer ccancel()

			act, err := p.upstream.Activation(cctx, acc.Session)
			if err != nil {
				rows[i].Error = err.Error()
				rows[i].SessionOK = false
				return
			}
			rows[i].Bound = act.Activated
			rows[i].Applied = act.AppliedCode

			codes, err := p.upstream.InvitationCodes(cctx, acc.Session)
			if err != nil {
				// activation 已拿到，码没拿到：仍可判定「能不能绑」之外的信息，
				// 但没有码就无法作为邀请方参与配对，标成不可配对。
				rows[i].Error = err.Error()
				return
			}
			rows[i].Codes = codes
			if len(codes) > 0 {
				rows[i].Code = codes[0].Code
			}
		}(i, acc)
	}
	wg.Wait()

	// 回写本地账本：上游是权威，本地字段只是快照（供离线预筛与展示）。
	for i, acc := range accounts {
		changed := false
		if rows[i].Code != "" && acc.InviteCode != rows[i].Code {
			acc.InviteCode, changed = rows[i].Code, true
		}
		if rows[i].Applied != "" && acc.InviteApplied != rows[i].Applied {
			acc.InviteApplied, changed = rows[i].Applied, true
		}
		// 上游 usedCount 是权威：本地 InviteUsed 只是预占账本（并发选码用），
		// 两边漂移会让「还剩几格」算错。只升不降——预占后上游还没结算时
		// usedCount 会小一次，降下来会导致超发。
		if len(rows[i].Codes) > 0 {
			if used := int(rows[i].Codes[0].UsedCount); used > acc.InviteUsed {
				acc.InviteUsed, changed = used, true
				rows[i].Used = used
			}
			if st := rows[i].Codes[0].Status; st != "" && st != "active" && !acc.InviteExhausted {
				acc.InviteExhausted, changed = true, true
				rows[i].Exhausted = true
			}
		}
		if changed {
			p.persist(acc)
		}
	}

	return rows, inviteSummary(rows, p.inviteMaxUses())
}

// inviteCandidate 池里一个还有余量的邀请码。
type inviteCandidate struct {
	Owner *pool.Account
	Code  string
	Used  int
}

// OnboardingAutoEnabled 新手任务补领是否开启。
//
// 它和邀请码兜底共用同一个周期，但**各自独立判断开关**：关掉新手任务补领
// 不该连带停掉邀请码扫描——后者是一次性资源，漏了没有任何补救机会。
func (p *Panel) OnboardingAutoEnabled() bool { return p.cfg.OnboardingAuto }

// inviteMaxUses 单个码能让几个人用（<=0 回落 5）。
//
// 这个值必须由配置给，不能读上游 maxUses：后者实测恒为 1000，
// 而客户端页面上写的是「每个邀请码限 N 人使用」，两者根本不是一回事。
func (p *Panel) inviteMaxUses() int {
	if p.cfg.InviteMaxUses > 0 {
		return p.cfg.InviteMaxUses
	}
	return 5
}

// inviteCodeOwner 按码反查持有者（池内）。
func (p *Panel) inviteCodeOwner(code string) *pool.Account {
	for _, a := range p.pool.All() {
		if a.InviteCode == code {
			return a
		}
	}
	return nil
}

// reserveInviteCode 挑一个还有余量的码并**当场占用一格**。
//
// 预占而不是「绑完再 ++」：周期扫描是 4 路并发，先查后写会让两个新号
// 同时认定「还剩最后一格」，结果第 6 个人也绑上了。占用失败则回滚。
//
// 挑选顺序：
//  1. 配置的 invite_code 优先（用户可能想把主号先灌满）；
//  2. 其余按 InviteUsed 升序——让 121 个码均匀分摊，而不是逮住一个灌到死；
//  3. 同 usedCount 按账号名排序，保证可复现。
//
// 排除：自己的码（绑自己无效）、已置 exhausted 的、用满上限的。
func (p *Panel) reserveInviteCode(exclude string) (inviteCandidate, bool) {
	p.invitePickMu.Lock()
	defer p.invitePickMu.Unlock()

	max := p.inviteMaxUses()
	pick := func(a *pool.Account) bool {
		return a != nil && a.Enabled && a.InviteCode != "" && !a.InviteExhausted &&
			a.InviteUsed < max && a.InviteCode != exclude
	}

	var best *pool.Account
	for _, a := range p.pool.All() {
		if !pick(a) {
			continue
		}
		if best == nil || a.InviteUsed < best.InviteUsed ||
			(a.InviteUsed == best.InviteUsed && a.Name < best.Name) {
			best = a
		}
	}
	if pref := strings.TrimSpace(p.cfg.InviteCode); pref != "" {
		if owner := p.inviteCodeOwner(pref); pick(owner) {
			best = owner
		}
	}
	if best == nil {
		return inviteCandidate{}, false
	}
	best.InviteUsed++
	p.persist(best)
	return inviteCandidate{Owner: best, Code: best.InviteCode, Used: best.InviteUsed}, true
}

// releaseInviteCode 归还预占的一格（绑定没成）。
func (p *Panel) releaseInviteCode(c inviteCandidate) {
	if c.Owner == nil {
		return
	}
	p.invitePickMu.Lock()
	defer p.invitePickMu.Unlock()
	if c.Owner.InviteUsed > 0 {
		c.Owner.InviteUsed--
	}
	p.persist(c.Owner)
}

// markInviteExhausted 上游明确拒绝了这个码：永久不再尝试，换下一个。
//
// 不靠猜上限，靠上游的实际拒绝：配 5 而真实是 3，第 4 次被拒这里就置位；
// 配 3 而真实是 5，只是白放着 2 格（下次把配置调大即可，不会重复撞）。
func (p *Panel) markInviteExhausted(c inviteCandidate, reason string) {
	if c.Owner == nil {
		return
	}
	p.invitePickMu.Lock()
	defer p.invitePickMu.Unlock()
	c.Owner.InviteExhausted = true
	p.persist(c.Owner)
	p.Logf("邀请码 %s（%s）已不可用，标记停用：%s", c.Code, c.Owner.Name, reason)
}

// autoBindInvite 账号进池后自动绑定一个还有余量的邀请码（被邀方 +5000）。
//
// 幂等三层，按代价从低到高排：
//  1. 本地 invite_applied 非空 → 直接返回，一个上游请求都不发；
//  2. 上游 activated → 只把「实际绑的是谁」回写本地，不再消耗；
//  3. 都没有才真绑——且最多换 3 个码，被上游拒绝一个就换下一个。
//
// 失败不重试网络错误：网络抖可以等下一个登录/扫描周期；被拒的码却是
// 终态，必须当场换掉，否则每加一个号都拿同一个废码去撞。
func (p *Panel) autoBindInvite(acc *pool.Account) (int64, error) {
	if !p.cfg.InviteAuto {
		return 0, nil
	}
	if acc == nil || !acc.Enabled || !acc.SessionValid() {
		return 0, nil
	}
	if acc.InviteApplied != "" {
		return 0, nil // 一次性资源已经用掉
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	act, err := p.upstream.Activation(ctx, acc.Session)
	if err != nil {
		return 0, err
	}
	if act.Activated {
		if act.AppliedCode != "" && acc.InviteApplied != act.AppliedCode {
			acc.InviteApplied = act.AppliedCode
			p.persist(acc)
		}
		return 0, nil
	}

	for attempt := 0; attempt < 3; attempt++ {
		cand, ok := p.reserveInviteCode(acc.InviteCode)
		if !ok {
			return 0, nil // 池内已无余量
		}

		bound, berr := p.upstream.BindInviteCode(ctx, acc.Session, cand.Code, acc.Identity.CampusDev)
		if berr != nil {
			p.releaseInviteCode(cand)
			var ie *upstream.InviteError
			if errors.As(berr, &ie) && ie.InviteRejectedCode() {
				p.markInviteExhausted(cand, ie.Desc)
				continue // 换下一个码
			}
			return 0, berr
		}
		if bound.AlreadyActivated && bound.InviteeReward == 0 {
			// 号自己已经绑过（别处绑的），码没问题：回滚占用并记账。
			p.releaseInviteCode(cand)
			if bound.AppliedCode != "" {
				acc.InviteApplied = bound.AppliedCode
				p.persist(acc)
			}
			return 0, nil
		}
		if !bound.Activated {
			p.markInviteExhausted(cand, "上游未返回 activated")
			continue
		}

		acc.InviteApplied = cand.Code
		acc.InviteBoundAt = time.Now().Unix()
		if bound.InviteeReward > 0 {
			acc.InviteReward += bound.InviteeReward
		}
		if len(bound.Codes) > 0 {
			acc.InviteCode = bound.Codes[0].Code
		}
		p.persist(acc)
		p.Logf("账号 %s 自动绑定邀请码 %s（%s 的第 %d/%d 次）成功，+%d 积分，出口 %s",
			acc.Name, cand.Code, cand.Owner.Name, cand.Used, p.inviteMaxUses(),
			bound.InviteeReward, p.egressTag(acc.Name))
		return bound.InviteeReward, nil
	}
	return 0, nil
}

// SweepInvites 给池里「还没花掉一次性邀请码」的号补绑配置里的码。
//
// 为什么不能只靠登录链路：导入 session 的卡密号根本不走登录（addAccount 里
// 只有 login=true 才登），它们的 5000 只会在这一轮里被发现。稳定后的池子
// 有 0 个待绑号 ⇒ 零上游请求，挂在高频 tick 上也不心疼。
func (p *Panel) SweepInvites(ctx context.Context) (accounts int, points int64) {
	// 只认开关：invite_code 现在是「优先码」，留空也照样从池里挑，
	// 所以不再用它当总闸（早先的版本会因此静默不跑）。
	if !p.cfg.InviteAuto {
		return 0, 0
	}
	defer func() {
		p.inviteSweepMu.Lock()
		p.inviteSweepLast = time.Now()
		p.inviteSweepBind = accounts
		p.inviteSweepPoints = points
		p.inviteSweepMu.Unlock()
	}()
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, 4)
	)
	for _, a := range p.pool.All() {
		if !a.Enabled || !a.SessionValid() || a.InviteApplied != "" || a.InviteCode == p.cfg.InviteCode {
			continue
		}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !p.tryLock(a.Name) {
				return // 正在登录/签到，下一轮再来
			}
			defer p.unlock(a.Name)
			pts, err := p.autoBindInvite(a)
			if err != nil {
				p.Logf("账号 %s 补绑邀请码失败%s：%v", a.Name, p.egressTag(a.Name), err)
				return
			}
			if pts > 0 {
				mu.Lock()
				accounts++
				points += pts
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	return accounts, points
}

// inviteSummary 顶部统计：多少号可绑、多少已绑、多少无码/异常、预期还能换多少分。
func inviteSummary(rows []inviteRow, inviteMaxUses int) map[string]any {
	var bindable, bound, noCode, failed int
	for _, r := range rows {
		switch {
		case r.Error != "" && !r.SessionOK:
			failed++
		case r.Bound:
			bound++
		case r.canBind():
			bindable++
		case r.Code == "":
			noCode++
		}
	}
	// 剩余「可被邀请」的格子：每个码上限 max，已用 used。这是加新号时真正
	// 能换钱的余额，比「多少个号还没绑」更接近用户关心的「还能薅多少」。
	max := inviteMaxUses
	var slots int
	for _, r := range rows {
		if r.Exhausted || r.Code == "" {
			continue
		}
		if left := max - r.Used; left > 0 {
			slots += left
		}
	}
	// pair 模式每次绑定同时让「被邀方」拿 5000，且邀请方也拿 5000 ——
	// 但邀请方的 5000 落在别人的号上，所以池子整体收益 = 绑定次数 × 10000。
	return map[string]any{
		"accounts":    len(rows),
		"bindable":    bindable,
		"bound":       bound,
		"no_code":     noCode,
		"failed":      failed,
		"reward_each": InviteRewardPerBind,
		"potential":   int64(bindable) * InviteRewardPerBind * 2,
		"max_uses":    max,
		"slots":       slots, // 还能再被多少人绑定（迎新号的总额度）
	}
}

// pairInvites 两两配对：按名字排序后相邻成对，A 绑 B 的码、B 绑 A 的码。
//
// 为什么排序而不按余额/出口：绑定不可逆，预览里用户确认过的 A↔B 必须就是
// 实际执行的 A↔B。按名字排序是唯一稳定且可复现的键（同批账号重跑同一份计划；
// 加一个新号只会插进它在字典序里的位置，不会把整张配对表打乱）。
//
// 奇数个时最后一个号绑 fallback（用户指定的主码），没有 fallback 就留空
// 交给用户决定——宁可少换 1 万，也不要替用户瞎猜。
func pairInvites(cands []inviteRow, fallback string) ([]InviteAssignment, []string) {
	sorted := make([]inviteRow, len(cands))
	copy(sorted, cands)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var (
		out      []InviteAssignment
		unpaired []string
	)
	for i := 0; i+1 < len(sorted); i += 2 {
		a, b := sorted[i], sorted[i+1]
		if a.Code == b.Code {
			// 理论上不会发生（每个号一个独立码）；真撞了就别互相绑，
			// 否则两条都白花。
			unpaired = append(unpaired, a.Name, b.Name)
			continue
		}
		out = append(out,
			InviteAssignment{Account: a.Name, Code: b.Code, From: b.Name, Note: "互绑"},
			InviteAssignment{Account: b.Name, Code: a.Code, From: a.Name, Note: "互绑"},
		)
	}
	if len(sorted)%2 == 1 {
		last := sorted[len(sorted)-1]
		if fb := strings.TrimSpace(fallback); fb != "" && fb != last.Code {
			out = append(out, InviteAssignment{Account: last.Name, Code: fb, Note: "奇数补位"})
		} else {
			unpaired = append(unpaired, last.Name)
		}
	}
	return out, unpaired
}

// targetInvites 所有候选号统一绑到 target 码。
func targetInvites(cands []inviteRow, target string) []InviteAssignment {
	out := make([]InviteAssignment, 0, len(cands))
	for _, r := range cands {
		if r.Code == target {
			continue // 不能绑自己的码
		}
		out = append(out, InviteAssignment{Account: r.Name, Code: target, Note: "统一绑码"})
	}
	return out
}

// filterCandidates 按名字白名单过滤出「确实还能绑」的号。
func filterCandidates(rows []inviteRow, names []string) []inviteRow {
	want := map[string]bool{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	out := make([]inviteRow, 0, len(rows))
	for _, r := range rows {
		if len(want) > 0 && !want[r.Name] {
			continue
		}
		if r.canBind() {
			out = append(out, r)
		}
	}
	return out
}

// invitesHandler GET /panel/api/invites：矩阵 + 统计。
func (p *Panel) invitesHandler(w http.ResponseWriter, r *http.Request) {
	rows, summary := p.collectInvites(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows,
		"summary":  summary,
		"job":      p.inviteJobView(),
		// 新号自动绑码的状态：面板顶部那条开关读它，保存走 POST /panel/api/config
		// （invite_auto / invite_code 是热生效字段，不需要重启）。
		"auto": p.inviteAutoInfo(),
	})
}

// inviteBindRequest POST /panel/api/invites/bind 的入参。
type inviteBindRequest struct {
	Mode    string   `json:"mode"`     // pair（两两互绑）| target（统一绑码）
	Target  string   `json:"target"`   // mode=target 必填；mode=pair 时作为奇数补位码
	Names   []string `json:"names"`    // 空 = 全池
	DelayMS int      `json:"delay_ms"` // 逐条间隔，默认 1200ms
	Confirm bool     `json:"confirm"`  // false = 只出计划（预览）
}

// inviteBindHandler POST /panel/api/invites/bind：预览或启动批量绑定。
//
// 预览与执行走同一个 buildPlan，保证「看到的」就是「要做的」。
// 执行是后台任务（118 条 × 1.2s ≈ 2.5 分钟，走同步响应必然超时），
// 前端轮询 /panel/api/invites/progress。
func (p *Panel) inviteBindHandler(w http.ResponseWriter, r *http.Request) {
	var req inviteBindRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数解析失败: " + err.Error()})
		return
	}
	if req.Mode == "" {
		req.Mode = "pair"
	}
	if req.Mode != "pair" && req.Mode != "target" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode 只能是 pair 或 target"})
		return
	}
	if req.Mode == "target" && strings.TrimSpace(req.Target) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode=target 时必须提供 target 邀请码"})
		return
	}
	if req.DelayMS <= 0 {
		req.DelayMS = inviteDefaultDelayMS
	}
	if req.DelayMS < 300 {
		req.DelayMS = 300 // 再快就是给上游送风控特征，不是「省时间」
	}

	rows, summary := p.collectInvites(r.Context())
	cands := filterCandidates(rows, req.Names)

	var (
		plan     []InviteAssignment
		unpaired []string
	)
	if req.Mode == "pair" {
		plan, unpaired = pairInvites(cands, strings.TrimSpace(req.Target))
	} else {
		plan = targetInvites(cands, strings.TrimSpace(req.Target))
	}

	if !req.Confirm {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run":  true,
			"mode":     req.Mode,
			"plan":     plan,
			"unpaired": unpaired,
			"summary":  summary,
			"estimate": int64(len(plan)) * InviteRewardPerBind,
		})
		return
	}
	if len(plan) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "plan": []any{}, "unpaired": unpaired,
			"message": "没有可绑定的账号（都已绑定过 / 无自持码 / 登录态失效）"})
		return
	}
	if !p.startInviteJob(plan, time.Duration(req.DelayMS)*time.Millisecond) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "已有绑定任务在跑，先等它结束"})
		return
	}
	p.Logf("邀请码：启动批量绑定 %d 条（mode=%s delay=%dms）", len(plan), req.Mode, req.DelayMS)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "total": len(plan), "unpaired": unpaired})
}

// inviteJob 批量绑定的运行态。同一时刻只允许一个任务。
type inviteJob struct {
	mu      sync.Mutex
	running bool
	delay   time.Duration
	total   int
	done    int
	skipped int
	failed  int
	gained  int64
	started time.Time
	ended   time.Time
	results []InviteResult
}

// startInviteJob 起一个后台任务；已有任务在跑时返回 false。
func (p *Panel) startInviteJob(plan []InviteAssignment, delay time.Duration) bool {
	p.inviteMu.Lock()
	if p.inviteJob != nil && p.inviteJob.running {
		p.inviteMu.Unlock()
		return false
	}
	job := &inviteJob{
		running: true, delay: delay, total: len(plan),
		started: time.Now(), results: make([]InviteResult, 0, len(plan)),
	}
	p.inviteJob = job
	p.inviteMu.Unlock()

	go p.runInviteJob(job, plan)
	return true
}

// runInviteJob 逐条执行「复查 → 绑定 → 落账」。
//
// 串行 + 间隔是故意的：并发绑定会让同一秒从同一台机器打出几十个
// POST /points/activation，这正是上游风控最想抓的形态；118 条串行
// 约 2.5 分钟，代价可以接受。
func (p *Panel) runInviteJob(job *inviteJob, plan []InviteAssignment) {
	ctx := context.Background()
	for _, as := range plan {
		p.runOneInvite(ctx, job, as)
		// 间隔放在循环末尾而不是成功分支里：失败/跳过同样会打上游
		// （复查 activation 就是一次 GET），只对成功限速等于没限速。
		if job.delay > 0 {
			time.Sleep(job.delay)
		}
	}

	job.mu.Lock()
	job.running = false
	job.ended = time.Now()
	total, ok, skipped, failed, gained := job.total, job.done, job.skipped, job.failed, job.gained
	job.mu.Unlock()
	p.Logf("邀请码：批量绑定结束——成功 %d / 跳过 %d / 失败 %d（共 %d），本次到账 %d 积分",
		ok, skipped, failed, total, gained)
}

// runOneInvite 执行一条赋值。
//
// 每条之前现场 GET 一次 activation：本地 InviteApplied 只是快照，用快照当判据
// 会在「别处绑过」时白打一次 POST（上游回 alreadyActivated，不扣分也没收益）。
// 这一步同时是幂等保险——任务被重复触发时不会把号的绑定机会浪费在死码上。
func (p *Panel) runOneInvite(ctx context.Context, job *inviteJob, as InviteAssignment) {
	res := InviteResult{Account: as.Account, Code: as.Code}
	acc := p.pool.Get(as.Account)
	if acc == nil {
		res.Reason = "账号不存在"
		p.recordInvite(job, res)
		return
	}
	if !acc.Enabled || !acc.SessionValid() {
		res.Reason = "账号已禁用或登录态失效"
		p.recordInvite(job, res)
		return
	}
	if !p.tryLock(acc.Name) {
		res.Reason = "该号正在执行其它运维动作（登录/签到），跳过"
		p.recordInvite(job, res)
		return
	}
	defer p.unlock(acc.Name)

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	act, err := p.upstream.Activation(cctx, acc.Session)
	if err != nil {
		res.Reason = "复查状态失败: " + err.Error()
		p.recordInvite(job, res)
		return
	}
	if act.Activated {
		res.Skipped = true
		res.Reason = "已绑定过 " + act.AppliedCode + "，不重复消耗"
		p.recordInvite(job, res)
		return
	}

	bound, berr := p.upstream.BindInviteCode(cctx, acc.Session, as.Code, acc.Identity.CampusDev)
	if berr != nil {
		res.Reason = berr.Error()
		p.recordInvite(job, res)
		return
	}
	if bound.AlreadyActivated && bound.InviteeReward == 0 {
		res.Skipped = true
		res.Reason = "上游回 alreadyActivated（此前已绑过）"
		p.recordInvite(job, res)
		return
	}

	res.OK = true
	res.InviteeReward = bound.InviteeReward
	res.Balance = bound.CurrentBalance

	// 本地账本：绑定不可逆，成功即记账。额度字段交给常规刷新，
	// 这里只落「邀请码」这一组，避免与 refreshQuota 抢同一份快照。
	acc.InviteApplied = as.Code
	acc.InviteBoundAt = time.Now().Unix()
	if res.InviteeReward > 0 {
		acc.InviteReward += res.InviteeReward
	}
	if len(bound.Codes) > 0 {
		acc.InviteCode = bound.Codes[0].Code
	}
	p.persist(acc)

	p.Logf("邀请码：%s 绑定 %s 成功（+%d 积分，余额 %d，出口 %s）",
		acc.Name, as.Code, res.InviteeReward, res.Balance, p.egressTag(acc.Name))
	p.recordInvite(job, res)
}

// recordInvite 记一条结果并推进进度。
func (p *Panel) recordInvite(job *inviteJob, res InviteResult) {
	job.mu.Lock()
	defer job.mu.Unlock()
	job.results = append(job.results, res)
	switch {
	case res.OK:
		job.done++
		job.gained += res.InviteeReward
	case res.Skipped:
		job.skipped++
	default:
		job.failed++
	}
}

// inviteAutoInfo 自动绑码任务的状态（开关 / 优先码 / 上限 / 上次扫描）。
//
// last_run 是这台机器上「任务还活着」的证据：池子稳定时每轮都是 0 个待办，
// 日志里不会有任何输出，只有一个时间戳能证明调度没停。
func (p *Panel) inviteAutoInfo() map[string]any {
	out := map[string]any{
		"enabled":  p.cfg.InviteAuto,
		"code":     p.cfg.InviteCode,
		"max_uses": p.inviteMaxUses(),
		"interval_hours": func() int {
			if p.cfg.OnboardingIntervalHours > 0 {
				return p.cfg.OnboardingIntervalHours
			}
			return 6
		}(),
	}
	p.inviteSweepMu.Lock()
	if !p.inviteSweepLast.IsZero() {
		out["last_run"] = p.inviteSweepLast.Format(time.RFC3339)
		out["last_bound"] = p.inviteSweepBind
		out["last_points"] = p.inviteSweepPoints
	}
	p.inviteSweepMu.Unlock()
	return out
}

// inviteJobView 进度快照（未跑过时 running=false、total=0）。
func (p *Panel) inviteJobView() map[string]any {
	p.inviteMu.Lock()
	job := p.inviteJob
	p.inviteMu.Unlock()
	if job == nil {
		return map[string]any{"running": false, "total": 0, "done": 0}
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	out := map[string]any{
		"running": job.running,
		"total":   job.total,
		"done":    job.done,
		"skipped": job.skipped,
		"failed":  job.failed,
		"gained":  job.gained,
		"results": job.results,
	}
	if !job.started.IsZero() {
		out["started"] = job.started.Format(time.RFC3339)
	}
	if !job.ended.IsZero() {
		out["ended"] = job.ended.Format(time.RFC3339)
	}
	return out
}

// inviteProgressHandler GET /panel/api/invites/progress：只读进度，不打上游。
func (p *Panel) inviteProgressHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.inviteJobView())
}
