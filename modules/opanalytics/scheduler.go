package opanalytics

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// dailyCronExpr 每日 01:30(部署机本地时钟)跑 T+1。日切到**报告时区**在 job 内
// 用 message.timestamp 纪元秒重算，与 cron 时钟无关，故跨时区部署无需改此表达式。
const dailyCronExpr = "30 1 * * *"

// Scheduler 看板预聚合 ETL 的每日定时调度器(仿 modules/backup/scheduler.go)。
type Scheduler struct {
	log.Log
	etl     *ETL
	cron    *cron.Cron
	entryID cron.EntryID
	mu      sync.Mutex
	started bool
}

// NewScheduler 创建调度器。
func NewScheduler(etl *ETL) *Scheduler {
	return &Scheduler{
		Log:  log.NewTLog("OpanalyticsScheduler"),
		etl:  etl,
		cron: cron.New(),
	}
}

// Start 启动调度器(幂等)。出错只返回 error，由调用方记日志，不 panic。
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	entryID, err := s.cron.AddFunc(dailyCronExpr, func() {
		s.Info("scheduled opanalytics ETL triggered", zap.String("cron", dailyCronExpr))
		if err := s.etl.RunYesterday(); err != nil {
			s.Error("scheduled opanalytics ETL failed", zap.Error(err))
		}
	})
	if err != nil {
		return err
	}

	s.entryID = entryID
	s.cron.Start()
	s.started = true
	s.Info("opanalytics scheduler started", zap.String("cron", dailyCronExpr))
	return nil
}

// Stop 停止调度器。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}
	s.cron.Stop()
	s.started = false
	s.Info("opanalytics scheduler stopped")
}
