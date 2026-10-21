// onboarding.go 任务中心：把「新手任务」从账号行内的一次性按钮，提升为
// 可观测的账号 × 任务矩阵。
//
// 蓝本 workbuddy2api 的「任务中心」是任务队列（扫描 → 排队 → 执行 → 领奖）；
// Loomy 只有一组 8 项新手任务（合计 10000 积分），没有按任务维度的执行链路，
// 所以这里保留同一个心智、按协议事实收缩：一行一个账号、一格一个任务，
// 状态来自上游 GET /onboarding/tasks，领取复用 ClaimOnboarding（先查后补，幂等）。
//
// 为什么值得单开一页：账号池视图只知道「这个号领过新手任务没有」，看不出
// 8 项里缺哪一项；而上游对每一项独立结算，某项失败不会影响其余，缺项在矩阵
// 里是可见的，否则只能靠翻日志猜。
package panel

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/yuanjie-ai/loomy2api/internal/pool"
	"github.com/yuanjie-ai/loomy2api/internal/upstream"
)

// onboardingTaskMeta 任务的展示元数据。Short 是矩阵里的短标签：
// 完整标题（「在技能广场安装一个技能」）放进 8 列会把表格撑爆。
type onboardingTaskMeta struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Short  string `json:"short"`
	Points int64  `json:"points"`
}

var onboardingShort = map[string]string{
	"first_message":    "首条消息",
	"pick_skill":       "选技能",
	"generate_ppt":     "生成 PPT",
	"set_schedule":     "定时任务",
	"install_skill":    "装技能",
	"configure_remote": "远程控制",
	"create_soul":      "创建搭子",
	"share_soul":       "分享搭子",
}

// onboardingCatalog 按固定顺序产出任务元数据（点数以本地表为准，理由见
// upstream.OnboardingPoints 的注释）。
func onboardingCatalog() []onboardingTaskMeta {
	out := make([]onboardingTaskMeta, 0, len(upstream.OnboardingOrder))
	for _, key := range upstream.OnboardingOrder {
		out = append(out, onboardingTaskMeta{
			Key:    key,
			Title:  upstream.OnboardingTitles[key],
			Short:  onboardingShort[key],
			Points: upstream.OnboardingPoints[key],
		})
	}
	return out
}

// onboardingMatrix GET /panel/api/onboarding：全池新手任务状态矩阵。
//
// 只读：不改账号状态、不领任务，因此可以随时刷新。并发 4 路查询上游，
// 单号 20s 超时——一个号卡住不会拖死整页；查询失败的行带 error 字段显形，
// 不与「一项都没做」混为一谈（蓝本对补偿查询失败的处理口径）。
func (p *Panel) onboardingMatrix(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	catalog := onboardingCatalog()
	accounts := p.pool.All()

	type row struct {
		Name      string          `json:"name"`
		Remark    string          `json:"remark,omitempty"`
		Enabled   bool            `json:"enabled"`
		SessionOK bool            `json:"session_ok"`
		Tasks     map[string]bool `json:"tasks"`
		Done      int             `json:"done"`
		Earned    int64           `json:"earned"`
		Error     string          `json:"error,omitempty"`
	}
	rows := make([]row, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)

	for i, acc := range accounts {
		tasks := make(map[string]bool, len(catalog))
		for _, meta := range catalog {
			tasks[meta.Key] = false
		}
		rows[i] = row{
			Name: acc.Name, Remark: acc.Remark,
			Enabled: acc.Enabled, SessionOK: acc.SessionValid(),
			Tasks: tasks,
		}
		// 不可查询的号不占上游配额，也不发请求：禁用是运维决定，失效要重登，
		// 两者都不该被一次「刷新」变成 8×N 个注定失败的请求。
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
			cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
			defer ccancel()
			tasks, earned, err := p.upstream.OnboardingTasks(cctx, acc.Session)
			if err != nil {
				rows[i].Error = err.Error()
				return
			}
			rows[i].Tasks = tasks
			rows[i].Earned = earned
			for _, done := range tasks {
				if done {
					rows[i].Done++
				}
			}
		}(i, acc)
	}
	wg.Wait()

	var done, earned int64
	queryable := 0
	for _, item := range rows {
		done += int64(item.Done)
		earned += item.Earned
		if item.Enabled && item.SessionOK {
			queryable++
		}
	}
	claimable := int64(queryable)*upstream.OnboardingTotal - earned
	if claimable < 0 {
		claimable = 0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":    catalog,
		"total":    upstream.OnboardingTotal,
		"accounts": rows,
		"summary": map[string]any{
			"accounts":    len(rows),
			"queryable":   queryable,
			"tasks_total": queryable * len(catalog),
			"tasks_done":  done,
			"earned":      earned,
			"claimable":   claimable,
		},
	})
}
