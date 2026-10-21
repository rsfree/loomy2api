// views.go 面板高级视图端点：积分构成 / 积分历史 / 暂停选号 / 短信登录。
//
// 交互蓝本对齐 workbuddy2api-panel，数据全部走 Loomy 真实端点：
//   - packages：逐号聚合 Quota 口径 + 流水来源 + first-login 邀请码，折算成蓝本「积分包」形态；
//   - credit_history：把 points/records 流水整理成「时间 × 账号 × 变动 × 前后余额」跨号账本；
//   - pause/resume：只退选号、不关保号任务（签到/刷额度照常）——与禁用是两种运维语义；
//   - sms/send + sms/login：验证码登录（官方通道 account.LoginBySms 透传）。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/loomy2api/internal/account"
	"github.com/yuanjie-ai/loomy2api/internal/pool"
	"github.com/yuanjie-ai/loomy2api/internal/upstream"
)

// ---------------------------------------------------------------- 积分构成

// quotaPackages Quota 快照 → 蓝本「包」形态：每日赠送（次日周期到期）+ 长期积分（不到期）。
// 扣费顺序（活动/兑换码 → 长期）是上游行为，这里只镜像余额拆分。
func quotaPackages(q *upstream.Quota) []map[string]any {
	pkgs := make([]map[string]any, 0, 3)
	if q.DailyQuota != nil && *q.DailyQuota > 0 {
		remain := int64(0)
		if q.Daily != nil && *q.Daily > 0 {
			remain = *q.Daily
		}
		end := ""
		if d, err := time.ParseInLocation("2006-01-02", q.CycleDate, time.Local); err == nil {
			end = d.AddDate(0, 0, 1).Format("2006-01-02") // 次日重置（上游口径）
		}
		pkgs = append(pkgs, map[string]any{
			"name": "每日赠送", "package_code": "daily",
			"size": *q.DailyQuota, "remain": remain, "used": *q.DailyQuota - remain,
			"created_at": q.CycleDate, "end_time": end,
		})
	}
	if q.Permanent != nil && *q.Permanent > 0 {
		remain := int64(0)
		if q.Balance != nil && *q.Balance > 0 {
			remain = *q.Balance
		}
		pkgs = append(pkgs, map[string]any{
			"name": "长期积分", "package_code": "permanent",
			"size": *q.Permanent, "remain": remain, "used": *q.Permanent - remain,
		})
	}
	return pkgs
}

// creditPackagesFromRecords 入账流水（credit 方向）按描述归并成「来源包」：
// 注册奖励 / 邀请 / 活动等一源一包。剩余记 0——这些积分已并入长期/每日口径，
// 再记剩余会双计。
func creditPackagesFromRecords(recs []map[string]any) []map[string]any {
	type agg struct {
		size    int64
		created string
	}
	byName := map[string]*agg{}
	var order []string
	for _, m := range recs {
		if sAny(m["direction"]) != "credit" {
			continue
		}
		desc := sAny(m["desc"])
		if desc == "" {
			desc = "入账"
		}
		a := byName[desc]
		if a == nil {
			a = &agg{created: sAny(m["time"])}
			byName[desc] = a
			order = append(order, desc)
		}
		a.size += i64Any(m["points"])
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		a := byName[k]
		out = append(out, map[string]any{
			"name": k, "package_code": "credit",
			"size": a.size, "remain": 0, "used": a.size,
			"created_at": a.created,
		})
	}
	return out
}

// packagesHandler 全池积分构成快照。逐号实时查上游（并发 ≤3，蓝本同构），
// 失败只标在该号上。
func (p *Panel) packagesHandler(w http.ResponseWriter, r *http.Request) {
	accounts := p.pool.All()
	type acctRow struct {
		UID             string           `json:"uid"`
		Nickname        string           `json:"nickname"`
		Remain          int64            `json:"remain"`
		Size            int64            `json:"size"`
		Packages        []map[string]any `json:"packages"`
		InvitationCodes []map[string]any `json:"invitation_codes,omitempty"`
		DailyCycleDate  string           `json:"daily_cycle_date,omitempty"`
		CheckinDone     bool             `json:"checkin_done"`
		QuotaUpdatedAt  int64            `json:"quota_updated_at"`
		Error           string           `json:"error,omitempty"`
	}
	out := make([]acctRow, len(accounts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := acctRow{UID: a.Name, Nickname: a.Name, Packages: []map[string]any{}}
			if !a.SessionValid() {
				row.Error = "登录态已失效，需重新登录"
				out[i] = row
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			q, err := p.upstream.Quota(ctx, a.Session)
			if err != nil {
				row.Error = err.Error()
				out[i] = row
				return
			}
			a.SetQuota(q)
			p.persist(a)
			row.Packages = quotaPackages(q)
			if obj, err := p.upstream.Points(ctx, a.Session, 1); err == nil {
				if recs, ok := obj["records"].([]map[string]any); ok {
					row.Packages = append(row.Packages, creditPackagesFromRecords(recs)...)
				}
			}
			for _, pk := range row.Packages {
				row.Size += i64Any(pk["size"])
			}
			if q.Available != nil {
				row.Remain = *q.Available // 官方可用总额优先（包拆分只是展示口径）
			}
			row.QuotaUpdatedAt = q.RefreshedAt
			row.DailyCycleDate = q.CycleDate
			row.CheckinDone = a.CheckinDone()
			if fl, err := p.upstream.FirstLogin(ctx, a.Session); err == nil {
				if data, ok := fl["data"].(map[string]any); ok {
					if codes, ok := data["invitationCodes"].([]any); ok {
						for _, c := range codes {
							if m, ok := c.(map[string]any); ok {
								row.InvitationCodes = append(row.InvitationCodes, m)
							}
						}
					}
				}
			}
			out[i] = row
		}(i, a)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// ---------------------------------------------------------------- 积分历史

// creditHistoryHandler 把上游流水整理成时间轴账本（面板「积分历史」表）。
// 与蓝本同构：time 取 RFC3339，debit 记负 delta，before/after 为变动前后余额。
func (p *Panel) creditHistoryHandler(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	uid := r.URL.Query().Get("uid")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type entry struct {
		TS      int64
		Account string
		Delta   int64
		Before  int64
		After   int64
		Desc    string
		Model   string
		Source  string
	}
	var all []entry
	for _, a := range p.pool.All() {
		if uid != "" && a.Name != uid {
			continue
		}
		if !a.Enabled || a.Paused || !a.SessionValid() {
			continue
		}
		obj, err := p.upstream.Points(ctx, a.Session, 2)
		if err != nil {
			continue
		}
		// upstream.Points 已把流水规范成 []map[string]any：time 是字符串秒、
		// points 恒正数、direction 记借贷向、balance_after 数值。
		recs, _ := obj["records"].([]map[string]any)
		for _, m := range recs {
			delta := i64Any(m["points"])
			if sAny(m["direction"]) == "debit" {
				delta = -delta
			}
			after := i64Any(m["balance_after"])
			all = append(all, entry{
				TS:      i64Any(sAny(m["time"])),
				Account: a.Name,
				Delta:   delta,
				Before:  after - delta,
				After:   after,
				Desc:    sAny(m["desc"]),
				Model:   sAny(m["model"]),
				Source:  sAny(m["source"]),
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS > all[j].TS })
	if len(all) > limit {
		all = all[:limit]
	}
	entries := make([]map[string]any, 0, len(all))
	for _, e := range all {
		ts := ""
		if e.TS > 0 {
			ts = time.Unix(e.TS, 0).Format(time.RFC3339)
		}
		entries = append(entries, map[string]any{
			"time": ts, "account": e.Account, "delta": e.Delta,
			"before": e.Before, "after": e.After, "desc": e.Desc,
			"model": e.Model, "source": e.Source,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "limit": limit})
}

func sAny(v any) string {
	s, _ := v.(string)
	return s
}

func i64Any(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// ---------------------------------------------------------------- 暂停选号

func (p *Panel) pauseAccount(w http.ResponseWriter, r *http.Request) {
	p.togglePause(w, r, true)
}

func (p *Panel) resumeAccount(w http.ResponseWriter, r *http.Request) {
	p.togglePause(w, r, false)
}

// togglePause 暂停/恢复选号。与禁用的区别：暂停的账号照常签到与刷额度（保号），
// 只是不接转发流量。
func (p *Panel) togglePause(w http.ResponseWriter, r *http.Request, paused bool) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	acc.Paused = paused
	p.persist(acc)
	verb := "暂停选号（签到/刷额度照常）"
	if !paused {
		verb = "恢复选号"
	}
	p.Logf("账号 %s 已%s", name, verb)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

// ---------------------------------------------------------------- 账号备注

// remarkMaxLen 备注长度上限（rune）。面板输入框 maxlength=24，后端再挡一道：
// 这个值会落进 auths/<账号>.json 并被渲染到账号池 / 用量 / 积分构成三个视图。
const remarkMaxLen = 24

// setRemark POST /panel/api/accounts/{name}/remark，body {"remark":"…"}。
//
// 备注存在账号文件（auths/<账号>.json 的 remark 键）里，跟着账号一起备份/迁移，
// 不再像早期版本那样只留在浏览器 localStorage（换浏览器、换入口、清站点数据即丢）。
// 空串表示清除：persisted() 对空值是「不写键」，所以清除后文件里不会留空字段。
func (p *Panel) setRemark(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	var body struct {
		Remark string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	remark := strings.TrimSpace(body.Remark)
	if n := len([]rune(remark)); n > remarkMaxLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("备注最长 %d 个字符（当前 %d）", remarkMaxLen, n))
		return
	}
	acc.Remark = remark
	p.persist(acc)
	verb := "已设置备注"
	if remark == "" {
		verb = "已清除备注"
	}
	p.Logf("账号 %s %s", name, verb)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}

// ---------------------------------------------------------------- 短信登录

// smsSendHandler 给账号绑定的手机号发验证码（复用官方登录通道）。
func (p *Panel) smsSendHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	acc := p.pool.Get(body.Name)
	phone := body.Phone
	if acc != nil && acc.LoginID != "" {
		phone = acc.LoginID
	}
	if phone == "" {
		writeErr(w, http.StatusBadRequest, "缺少手机号")
		return
	}
	ident := account.NewIdentity()
	if acc != nil && acc.Identity.DevID != "" {
		ident = acc.Identity
	}
	out, err := p.accounts.SendSmsCode(phone, ident)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// checkCode 需要 msgid 做二次校验；信封 data 里形态宽容提取。
	msgID := ""
	if data, ok := out["data"].(map[string]any); ok {
		msgID = sAny(data["msgId"])
		if msgID == "" {
			msgID = sAny(data["msgid"])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg_id": msgID})
}

// smsLoginHandler 验证码登录并写回账号。
func (p *Panel) smsLoginHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
		Code  string `json:"code"`
		MsgID string `json:"msg_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	if body.Phone == "" || body.Code == "" {
		writeErr(w, http.StatusBadRequest, "需要 phone + code")
		return
	}
	acc := p.pool.Get(body.Name)
	if acc == nil {
		if body.Name == "" {
			writeErr(w, http.StatusBadRequest, "新账号需要 name")
			return
		}
		acc = &pool.Account{Name: body.Name, LoginID: body.Phone, Enabled: true}
	}
	ident := account.NewIdentity()
	if acc.Identity.DevID != "" {
		ident = acc.Identity
	}
	res, err := p.accounts.LoginBySms(body.Phone, body.Code, body.MsgID, ident)
	if err != nil {
		acc.SetLastError("短信登录失败: " + err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	acc.Identity = ident
	acc.Session = res.Session
	acc.UserID = res.UserID
	acc.ObtainedAt = time.Now().Unix()
	acc.ExpireAt = acc.ObtainedAt + account.SessionExpireSeconds
	acc.Enabled = true
	acc.Paused = false
	acc.ResetCooldowns()
	p.pool.Add(acc)
	p.persist(acc)
	p.Logf("账号 %s 短信登录成功（14 天有效）", acc.Name)
	go p.checkinAndRefresh(acc)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acc.Public()})
}
