// main.go loomy2api 入口：加载配置、构建账号池与面板、起 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rsfree/loomy2api/internal/account"
	"github.com/rsfree/loomy2api/internal/config"
	"github.com/rsfree/loomy2api/internal/egress"
	"github.com/rsfree/loomy2api/internal/panel"
	"github.com/rsfree/loomy2api/internal/pool"
	"github.com/rsfree/loomy2api/internal/server"
	"github.com/rsfree/loomy2api/internal/upstream"
	"github.com/rsfree/loomy2api/internal/usage"
)

// appVersion 面板版本，发布构建由 CI 用 -X 注入（形如 2026.10.19.1430）。
var appVersion = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（不存在时自动生成推荐配置）")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	loadedFrom := *cfgPath
	// Load 对「文件不存在」是宽容的（返回默认值并非错误），所以首启判断要显式做。
	if _, statErr := os.Stat(*cfgPath); errors.Is(statErr, fs.ErrNotExist) {
		if path, key, werr := config.Ensure(*cfgPath); werr == nil {
			log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", path, key)
		} else {
			log.Printf("config %s 不存在且自动生成失败（将用默认+环境变量）: %v", *cfgPath, werr)
			loadedFrom = ""
		}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if loadedFrom == "" {
		log.Printf("未读取配置文件，当前生效的是内置默认值 + 环境变量")
	}

	// ---- 账号池 ----
	stateFile := cfg.Resolve(cfg.StateFile)
	p := pool.New(stateFile)
	defer p.Close()

	authDir := cfg.Resolve(cfg.AuthDir)
	if accounts, lerr := pool.LoadAccounts(authDir); lerr == nil && len(accounts) > 0 {
		p.SyncToDir(accounts)
		p.ApplyState()
		log.Printf("从 %s 加载 %d 个账号", authDir, len(accounts))
	} else if lerr != nil {
		log.Printf("读取账号目录 %s 失败: %v", authDir, lerr)
	} else {
		p.ApplyState()
	}
	p.SetOptions(panel.PoolOptions(cfg))

	// ---- 上游与账号服务客户端 ----
	upTimeout := time.Duration(cfg.TimeoutSecond) * time.Second
	up := upstream.NewClient(cfg.Upstream, cfg.LoomyVersion, cfg.Proxy, upTimeout)

	akID, akSecret := cfg.AccessKey()
	accClient := account.NewClient(cfg.AccountBase, cfg.AccountAppID, akID, akSecret, cfg.Proxy)

	// ---- 出口隔离：账号 → 本机源 IP ----
	// 账号集合（而不是请求）决定分配，所以启动后先算一遍；之后面板增删账号
	// 通过 OnChange 增量重算。未配置 mode=local 时保持单出口，行为与旧版一致。
	eg := egress.New(cfg.Egress)
	if eg.Enabled() {
		log.Printf("出口隔离：%s（按账号 hash 分配）", eg.Describe())
	} else if cfg.Egress.Mode == egress.ModeLocal {
		log.Printf("⚠️  出口隔离未生效：%v", eg.Error())
	}
	up.SetEgress(eg, p.SessionOwner)
	accClient.Egress = eg
	eg.Rebuild(p.Names())
	p.OnChange = func() { eg.Rebuild(p.Names()) }

	// ---- 台账 ----
	usagePath := stateSibling(stateFile, "usage.jsonl")
	ledger := usage.New(usagePath, cfg.UsageMaxEntries)
	defer ledger.Close()
	ledger.Prune(30)

	// ---- 网关 ----
	h := server.New(cfg, p, up, ledger)
	h.Egress = eg
	server.Version = appVersion

	// ---- 面板 ----
	pl := panel.New(cfg, p, accClient, up, ledger, h.Models, appVersion)
	pl.SetEgress(eg)
	// log 包输出镜像进面板日志环：请求表格行（`| #001 |…`）按 chat 频道归类，
	// 面板「运行日志」因此能看到对话流水而不只是面板自己的动作。
	log.SetOutput(io.MultiWriter(os.Stderr, pl.LogWriter()))

	mux := http.NewServeMux()
	mux.Handle("/panel/", pl)
	mux.Handle("/", h.Routes())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		// 入站无总读写超时：流式对话可能持续几十分钟。
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("Loomy2API 网关启动：http://%s  （面板 %s/panel/，版本 %s）",
			normalizeAddr(cfg.Listen), "http://"+normalizeAddr(cfg.Listen), appVersion)
		if len(cfg.APIKeys) == 0 {
			log.Printf("⚠️  api_key 为空：/v1/* 与 /panel/api/* 均不鉴权，请勿暴露到公网")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	// 后台周期任务：额度刷新 + 会话粘性 GC + 自动续期提醒。
	stopCh := make(chan struct{})
	go background(cfg, p, up, pl, stopCh)

	// 模型目录预热：knownModel（裸 id 不补前缀）与图像模型拦截都依赖目录元数据，
	// 冷缓存下两者都会误判，而上游对认不出的模型名是静默回落到默认模型
	// （不报错、扣默认模型的积分）。异步执行，失败不阻塞启动。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if n := h.WarmCatalog(ctx); n > 0 {
			log.Printf("模型目录预热：%d 个模型", n)
		}
	}()

	// ---- 优雅退出 ----
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在停止…")
	close(stopCh)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	p.Flush()
	_ = loadedFrom
}

// background 周期任务：额度刷新、粘性 GC、登录态即将过期提醒。
func background(cfg *config.Config, p *pool.Pool, up *upstream.Client, pl *panel.Panel, stop <-chan struct{}) {
	quota := time.NewTicker(time.Duration(maxInt(cfg.QuotaRefreshMinutes, 1)) * time.Minute)
	defer quota.Stop()
	// 新手任务补领：启动 90s 后先跑一轮（刚导入的号不用等一个周期），
	// 之后挂在额度 ticker 上，按 onboarding_interval_hours 节流。
	onboardEvery := time.Duration(maxInt(cfg.OnboardingIntervalHours, 1)) * time.Hour
	lastOnboard := time.Now()
	var firstOnboard <-chan time.Time
	if cfg.OnboardingAuto {
		firstOnboard = time.After(90 * time.Second)
	}
	gc := time.NewTicker(5 * time.Minute)
	defer gc.Stop()
	// expiredWarned 记录每个账号最近一次登录态告警日期，避免同一账号反复刷屏。
	expiredWarned := map[string]string{}

	for {
		select {
		case <-stop:
			return
		case <-gc.C:
			if n := p.Sticky().Prune(); n > 0 {
				log.Printf("会话粘性：清理 %d 条过期绑定", n)
			}
			// 登录态即将过期提醒（Loomy 没有 refresh token，只能重登）。
			// 阈值取 config.session_renew_before_days；每个账号每天只提醒一次，
			// 否则 5 分钟一轮会把日志刷满。
			warnDays := cfg.SessionRenewDays
			if warnDays <= 0 {
				warnDays = 3
			}
			today := time.Now().Format("2006-01-02")
			for _, a := range p.All() {
				if !a.Enabled {
					continue
				}
				d := a.DaysLeft()
				if d < 0 || d >= float64(warnDays) {
					continue
				}
				if expiredWarned[a.Name] == today {
					continue
				}
				expiredWarned[a.Name] = today
				log.Printf("⚠️  账号 %s 登录态将在 %.1f 小时内过期（阈值 %d 天），请重新登录", a.Name, d*24, warnDays)
			}
			// 有密码的账号直接自动重登，把 14 天有效期滚动续上；
			// 纯 session 的卡密号没有 refresh token，只能等告警后换卡。
			if ok, bad := pl.AutoRenew(time.Now()); ok+bad > 0 {
				log.Printf("登录态自动续期：成功 %d，失败 %d", ok, bad)
			}
		case <-firstOnboard:
			runOnboarding(pl)
			lastOnboard = time.Now()
			firstOnboard = nil // nil channel 在 select 里永久阻塞 = 只跑一次
		case <-quota.C:
			refreshQuotas(p, up)
			// 周期到了就跑一轮运维（内部按各自开关决定做哪几件事），
			// 不再用 onboarding_auto 一个开关卡住整轮。
			if time.Since(lastOnboard) >= onboardEvery {
				runOnboarding(pl)
				lastOnboard = time.Now()
			}
		}
	}
}

// runOnboarding 跑一轮「账号运维」：新手任务补领 + 一次性邀请码兜底。
//
// 两件事共用同一个周期，但触发理由不同：新手任务漏了能补，邀请码是一次性的
// （一个号只能绑一次），所以邀请码在登录链路里就即时绑掉，这里只兜「不走登录
// 的号」（导入 session 的卡密号）。池子稳定后待绑数为 0，这一轮是纯本地遍历。
func runOnboarding(pl *panel.Panel) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if pl.OnboardingAutoEnabled() {
		if n, pts := pl.AutoOnboarding(ctx); n > 0 {
			log.Printf("新手任务自动补领：%d 个账号到账 %d 积分", n, pts)
		}
	}
	// 邀请码兜底：只处理「还没花掉一次性名额」的号，池子稳定时为 0 个待办、
	// 零上游请求，所以挂在同一个周期上不心疼。
	if b, ipts := pl.SweepInvites(ctx); b > 0 {
		log.Printf("邀请码自动绑定：%d 个账号到账 %d 积分", b, ipts)
	}
}

// refreshQuotas 并发刷新全池额度，并让余额恢复的账号自动解冻。
func refreshQuotas(p *pool.Pool, up *upstream.Client) {
	sem := make(chan struct{}, 4)
	done := make(chan struct{})
	count := 0
	for _, a := range p.All() {
		if !a.Enabled || !a.SessionValid() {
			continue
		}
		count++
		go func(a *pool.Account) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			q, err := up.Quota(ctx, a.Session)
			if err != nil {
				return
			}
			before := a.AvailablePoints()
			prevCycle := a.DailyCycleDate
			a.SetQuota(q)
			after := a.AvailablePoints()
			// Quota 内部已调幂等 first-login：跨日自动刷新每日积分。
			if q.CycleDate != "" && prevCycle != "" && q.CycleDate != prevCycle {
				log.Printf("账号 %s 进入新每日周期 %s（每日 %d，已自动签到刷新）", a.Name, q.CycleDate, derefI64(q.DailyQuota))
			}
			// 余额从耗尽恢复 → 自动解冻硬冷却。
			if before != nil && after != nil && *before <= 0 && *after > 0 {
				a.ResetCooldowns()
				log.Printf("账号 %s 余额恢复（%d），已自动解冻", a.Name, *after)
			}
		}(a)
	}
	for i := 0; i < count; i++ {
		<-done
	}
}

func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func normalizeAddr(addr string) string {
	if addr == "" {
		return "127.0.0.1:17890"
	}
	if addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// derefI64 安全解引用 *int64（nil → 0），仅用于日志。
func derefI64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
