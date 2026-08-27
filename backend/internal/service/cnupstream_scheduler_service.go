package service

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/cnupstream/auth"
	"github.com/Wei-Shaw/sub2api/internal/cnupstream/scheduler"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

// CnUpstreamSchedulerService 定时签到/保活调度器：按 config.Checkin 驱动各平台 Scheduler。
type CnUpstreamSchedulerService struct {
	cn     *CnUpstreamService
	cfg    *config.Config
	cancel context.CancelFunc
}

// NewCnUpstreamSchedulerService 构建调度器服务，Run 由 Start 启动。
func NewCnUpstreamSchedulerService(cn *CnUpstreamService, cfg *config.Config) *CnUpstreamSchedulerService {
	return &CnUpstreamSchedulerService{cn: cn, cfg: cfg}
}

// Start 加载账号并启动 workbuddy/traework 的签到保活调度器；qoder 无签到活动故跳过。
// ctx 取消时调度器退出；config.Checkin.Enabled 关闭时不启动。
// 调度器通过钩子回写 ent：token 刷新（PersistAuth）、签到结果（observer 落库），
// 批量任务前 Reload 感知管理端新增/删除账号。
func (s *CnUpstreamSchedulerService) Start(ctx context.Context) {
	if s == nil || s.cfg == nil || !s.cfg.Checkin.Enabled {
		log.Printf("cnupstream scheduler disabled")
		return
	}
	// 从 ent 账号库加载账号进池，否则调度器无号可签。
	if err := s.cn.ReloadAccounts(ctx); err != nil {
		log.Printf("cnupstream scheduler: reload accounts failed: %v", err)
		return
	}

	cctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	for _, platform := range []string{PlatformWorkBuddy, PlatformTraeWork} {
		p := s.cn.PlatformPool(platform)
		up := s.cn.PlatformUpstream(platform)
		if p == nil || up == nil {
			continue
		}
		sched := scheduler.New(s.schedulerConfig(platform, cctx))
		sched.SetCheckinObserver(s.checkinObserver(platform))
		go sched.Run(cctx)
		log.Printf("cnupstream scheduler started platform=%s", platform)
	}
}

// schedulerConfig 组装单平台调度器配置：签到时间、保活小时、退避间隔与
// ent 回写钩子（PersistAuth / Reload）。
func (s *CnUpstreamSchedulerService) schedulerConfig(platform string, ctx context.Context) scheduler.Config {
	return scheduler.Config{
		Pool:           s.cn.PlatformPool(platform),
		Upstream:       s.cn.PlatformUpstream(platform),
		Name:           platform,
		CheckinMinutes: parseCheckinMinutes(s.cfg.Checkin.Times),
		KeepaliveHours: s.cfg.Checkin.KeepaliveHours,
		CheckinBackoff: time.Duration(s.cfg.Checkin.BackoffSeconds) * time.Second,
		PersistAuth:    func(a *auth.Auth) error { return s.cn.PersistAuth(a) },
		Reload: func() {
			if err := s.cn.ReloadAccounts(ctx); err != nil {
				log.Printf("cnupstream scheduler: reload accounts failed: %v", err)
			}
		},
	}
}

// checkinObserver 返回按平台落库签到结果的观察者（积分 + lastCheckin 状态）。
func (s *CnUpstreamSchedulerService) checkinObserver(platform string) func(scheduler.CheckinResult) {
	return func(r scheduler.CheckinResult) {
		s.cn.PersistCheckinResult(platform, r)
	}
}

// Stop 取消调度上下文并停止各平台调度器。
func (s *CnUpstreamSchedulerService) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.cancel = nil
	log.Printf("cnupstream scheduler stopped")
}

// parseCheckinMinutes 把 "HH:MM" 时间配置解析为当天分钟数（0..1439），去重排序。
func parseCheckinMinutes(times []string) []int {
	seen := map[int]bool{}
	var out []int
	for _, t := range times {
		parts := strings.SplitN(t, ":", 2)
		if len(parts) != 2 {
			continue
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			continue
		}
		v := h*60 + m
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
