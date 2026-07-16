package thread

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"

	"go.uber.org/zap"
)

// archiveDB 抽出 ArchiveWorker 需要的最小 DB 接口，便于单测 mock。
type archiveDB interface {
	ArchiveStaleBatch(threshold time.Time, batchSize int, version int64, channelType uint8) (int64, error)
}

// versionGen 抽出版本号生成，便于单测注入确定性版本号。
type versionGen interface {
	GenSeq(key string) (int64, error)
}

// ArchiveWorker 周期性扫描 thread 表，把过期 active 子区切到 archived。
type ArchiveWorker struct {
	cfg ArchiveConfig
	db  archiveDB
	gen versionGen
	now func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	log.Log
}

// NewArchiveWorker 构造 worker。生产路径用 thread.NewDB 和 config.Context 注入。
func NewArchiveWorker(ctx *config.Context, cfg ArchiveConfig) *ArchiveWorker {
	return &ArchiveWorker{
		cfg: cfg,
		db:  NewDB(ctx),
		gen: ctx,
		now: time.Now,
		Log: log.NewTLog("ThreadArchiveWorker"),
	}
}

// Start 启动后台 ticker。Enabled=false 或 Interval 非法时不启动新 goroutine，但仍会
// 先停掉可能存在的旧 goroutine——避免未来热更新（enabled: true→false 再 Start）
// 留下孤儿 ticker。
// 重复调用幂等：先 stop 旧 goroutine 再启动新的。
//
// 启动门不再卡 Threshold：Threshold=0 是 config 明文支持的模式（DM_THREAD_AUTO_ARCHIVE_DAYS=0
// → 禁用时间归档但 Enabled 仍为 true）。此时 ticker 照常起，RunOnce 内部的 Threshold<=0
// 短路让每轮空转；行为可预测，且为将来 worker 承载其它时间无关动作预留余地。
func (w *ArchiveWorker) Start(ctx context.Context) {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
		w.cancel = nil
	}
	if !w.cfg.Enabled || w.cfg.Interval <= 0 {
		w.Info("thread auto-archive worker disabled",
			zap.Bool("enabled", w.cfg.Enabled),
			zap.Duration("interval", w.cfg.Interval),
			zap.Duration("threshold", w.cfg.Threshold))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		w.Info("thread auto-archive worker started",
			zap.Duration("interval", w.cfg.Interval),
			zap.Duration("threshold", w.cfg.Threshold),
			zap.Int("batch_size", w.cfg.BatchSize),
			zap.Duration("batch_sleep", w.cfg.BatchSleep))
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				archived, err := w.RunOnce(rctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					w.Error("thread auto-archive run failed", zap.Error(err))
					continue
				}
				if archived > 0 {
					w.Info("thread auto-archive run", zap.Int64("archived", archived))
				}
			}
		}
	}()
}

// Stop 通知 worker 退出并等待当前 RunOnce 跑完。
func (w *ArchiveWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// RunOnce 执行一轮归档循环：批量 UPDATE 直到一批返回 < batchSize 或 ctx 取消。
// 返回本轮累计归档行数。
//
// 安全保护：threshold<=0 / batchSize<=0 视为禁用，直接返回 (0, nil)；
// ctx 取消时返回 ctx.Err() 让上层日志可区分"正常停机"vs"异常"。
func (w *ArchiveWorker) RunOnce(ctx context.Context) (int64, error) {
	if w.cfg.Threshold <= 0 || w.cfg.BatchSize <= 0 {
		return 0, nil
	}
	cutoff := w.now().Add(-w.cfg.Threshold)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		version, err := w.gen.GenSeq(ThreadSeqKey)
		if err != nil {
			return total, err
		}
		rows, err := w.db.ArchiveStaleBatch(cutoff, w.cfg.BatchSize, version, common.ChannelTypeCommunityTopic.Uint8())
		if err != nil {
			return total, err
		}
		total += rows
		if rows < int64(w.cfg.BatchSize) {
			return total, nil
		}
		if w.cfg.BatchSleep > 0 {
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(w.cfg.BatchSleep):
			}
		}
	}
}
