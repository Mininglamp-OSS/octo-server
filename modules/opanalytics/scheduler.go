package opanalytics

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// dailyCronExpr 每日 01:30(部署机本地时钟)触发增量 ETL。日切到**报告时区**在聚合内
// 用 message.timestamp 纪元秒重算，与 cron 时钟无关，故跨时区部署无需改此表达式。
const dailyCronExpr = "30 1 * * *"

// Scheduler 看板增量 ETL 的每日定时调度器(仿 modules/backup/scheduler.go)。
// 多副本部署时由 Redis 锁保证同一时刻只有一个实例真正执行(验收④)。
type Scheduler struct {
	log.Log
	etl     *ETL
	lock    *etlLock
	cron    *cron.Cron
	entryID cron.EntryID
	mu      sync.Mutex
	started bool
}

// NewScheduler 创建调度器。
func NewScheduler(ctx *config.Context, etl *ETL) *Scheduler {
	return &Scheduler{
		Log:  log.NewTLog("OpanalyticsScheduler"),
		etl:  etl,
		lock: newETLLock(ctx),
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

	entryID, err := s.cron.AddFunc(dailyCronExpr, s.runOnce)
	if err != nil {
		return err
	}

	s.entryID = entryID
	s.cron.Start()
	s.started = true
	s.Info("opanalytics scheduler started", zap.String("cron", dailyCronExpr))
	return nil
}

// runOnce 单次触发：抢分布式锁，抢到才执行增量 ETL，结束释放锁。
func (s *Scheduler) runOnce() {
	token, acquired, err := s.acquireRunLock()
	if err != nil {
		// Redis 故障:不强行执行(避免多实例同跑);等下一 tick。
		s.Error("opanalytics ETL: acquire lock failed, skip this tick", zap.Error(err))
		return
	}
	if !acquired {
		s.Info("opanalytics ETL: lock held by another instance, skip")
		return
	}

	s.runWithHeldLock(token, "scheduled", zap.String("cron", dailyCronExpr))
}

// TriggerManualRun 抢同一把 ETL 分布式锁，抢到后异步跑一轮增量 ETL。
// 返回 (false,nil) 表示已有 cron 或手动任务在运行。
func (s *Scheduler) TriggerManualRun() (bool, error) {
	token, acquired, err := s.acquireRunLock()
	if err != nil || !acquired {
		return acquired, err
	}

	go s.runWithHeldLock(token, "manual")
	return true, nil
}

func (s *Scheduler) acquireRunLock() (string, bool, error) {
	token, err := randomToken()
	if err != nil {
		return "", false, err
	}
	acquired, err := s.lock.Acquire(token)
	if err != nil {
		return "", false, err
	}
	return token, acquired, nil
}

func (s *Scheduler) runWithHeldLock(token, source string, fields ...zap.Field) {
	done := make(chan struct{})
	go s.renewLockUntilDone(token, source, done)
	defer func() {
		close(done)
		if rerr := s.lock.Release(token); rerr != nil {
			s.Error("opanalytics ETL: release lock failed", zap.Error(rerr))
		}
	}()

	fields = append([]zap.Field{zap.String("source", source)}, fields...)
	s.Info("opanalytics ETL triggered", fields...)
	if err := s.etl.RunIncremental(); err != nil {
		s.Error("opanalytics ETL failed", append(fields, zap.Error(err))...)
		return
	}
	s.Info("opanalytics ETL finished", fields...)
}

func (s *Scheduler) renewLockUntilDone(token, source string, done <-chan struct{}) {
	interval := etlRunLockTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			renewed, err := s.lock.Renew(token)
			if err != nil {
				s.Error("opanalytics ETL: renew lock failed", zap.String("source", source), zap.Error(err))
				continue
			}
			if !renewed {
				s.Error("opanalytics ETL: lock ownership lost, stop renewing", zap.String("source", source))
				return
			}
		}
	}
}

// Stop 停止调度器并释放锁连接。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}
	s.cron.Stop()
	s.started = false
	if err := s.lock.Close(); err != nil {
		s.Error("opanalytics scheduler: close lock failed", zap.Error(err))
	}
	s.Info("opanalytics scheduler stopped")
}

// randomToken 生成锁持有者 token(每次抢锁新生成，供 CAS-DEL 释放校验)。
func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
