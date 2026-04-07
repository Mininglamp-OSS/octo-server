package backup

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Scheduler 定时调度器
type Scheduler struct {
	log.Log
	service *Service
	cron    *cron.Cron
	entryID cron.EntryID
	mu      sync.Mutex
	started bool
}

// NewScheduler 创建调度器
func NewScheduler(service *Service) *Scheduler {
	return &Scheduler{
		Log:     log.NewTLog("BackupScheduler"),
		service: service,
		cron:    cron.New(),
	}
}

// Start 启动调度器
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	cfg, err := s.service.GetConfig()
	if err != nil {
		s.Warn("failed to get backup config", zap.Error(err))
		return nil
	}

	if cfg == nil || !cfg.Enabled {
		s.Info("backup is disabled, scheduler not started")
		return nil
	}

	// 添加定时任务（走 TriggerBackup 确保并发检查）
	entryID, err := s.cron.AddFunc(cfg.CronExpr, func() {
		s.Info("scheduled backup triggered", zap.String("cron", cfg.CronExpr))
		if _, err := s.service.TriggerBackup(); err != nil {
			s.Error("scheduled backup skipped or failed", zap.Error(err))
		}
	})
	if err != nil {
		return err
	}

	s.entryID = entryID
	s.cron.Start()
	s.started = true

	s.Info("backup scheduler started", zap.String("cron", cfg.CronExpr))
	return nil
}

// Stop 停止调度器
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return
	}

	s.cron.Stop()
	s.started = false
	s.Info("backup scheduler stopped")
}

// UpdateSchedule 更新调度计划
func (s *Scheduler) UpdateSchedule(cronExpr string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 移除旧任务
	if s.entryID != 0 {
		s.cron.Remove(s.entryID)
		s.entryID = 0
	}

	if !enabled {
		s.Info("backup disabled, scheduler stopped")
		return nil
	}

	// 添加新任务（走 TriggerBackup 确保并发检查）
	entryID, err := s.cron.AddFunc(cronExpr, func() {
		s.Info("scheduled backup triggered", zap.String("cron", cronExpr))
		if _, err := s.service.TriggerBackup(); err != nil {
			s.Error("scheduled backup skipped or failed", zap.Error(err))
		}
	})
	if err != nil {
		return err
	}

	s.entryID = entryID

	// 如果 cron 未启动，启动它
	if !s.started {
		s.cron.Start()
		s.started = true
	}

	s.Info("backup schedule updated", zap.String("cron", cronExpr))
	return nil
}

// ValidateCronExpr 验证 cron 表达式
func ValidateCronExpr(cronExpr string) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	_, err := parser.Parse(cronExpr)
	return err
}

// GetNextRun 获取下次执行时间
func (s *Scheduler) GetNextRun() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.entryID == 0 {
		return ""
	}

	entry := s.cron.Entry(s.entryID)
	if entry.ID == 0 {
		return ""
	}

	return entry.Next.Format("2006-01-02 15:04:05")
}
