package group

import (
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// #394 孤儿群 reconcile 调参。
const (
	// reconcileCronExpr reconcile 扫描频率（每分钟）。孤儿是低频事件，分钟级足够把
	// 「有 group 行、无 IM 频道」的窗口收敛到最终一致，同时不给 DB/IM 压力。
	reconcileCronExpr = "@every 1m"

	// reconcileGrace 宽限期：只处理 updated_at 早于 now-grace 的 outbox 行。正常建群在
	// tx.Commit() 与 IM 创建之间会短暂留有 outbox 行（随即被删），宽限期保证这些 in-flight
	// 行不被误当孤儿重复建频道。也是失败行的退避基准（markChannelOutboxAttempt 推进 updated_at）。
	reconcileGrace = 2 * time.Minute

	// reconcileBatch 单轮最多处理的 outbox 行数，防止积压时一轮打爆 IM/DB；剩余的下一轮继续。
	reconcileBatch = 200
)

// channelReconciler 扫描 group_channel_outbox，为「已提交但 IM 频道未确认」的群幂等重建频道
// （或清理已消失群的陈旧 outbox 行），收口 #394 的孤儿群窗口。
//
// createChannel / subscribersOf 以字段注入，生产走真实 WuKongIM + 成员表；测试可注入桩，
// 无需 live WuKongIM 即可覆盖两种失败模式的恢复路径。
type channelReconciler struct {
	log.Log
	db            *DB
	grace         time.Duration
	batch         int
	createChannel func(*config.ChannelCreateReq) error
	subscribersOf func(groupNo string) ([]string, error)
}

func newChannelReconciler(ctx *config.Context) *channelReconciler {
	db := NewDB(ctx)
	return &channelReconciler{
		Log:           log.NewTLog("GroupChannelReconciler"),
		db:            db,
		grace:         reconcileGrace,
		batch:         reconcileBatch,
		createChannel: ctx.IMCreateOrUpdateChannel,
		subscribersOf: db.querySubscribableMemberUIDsWithGroupNo,
	}
}

// reconcileOnce 处理一批到期 outbox 行。返回本轮 (found 扫到, resolved 已收口, failed 本轮未成功)。
// 全程幂等，可安全重复运行 / 多实例并发（IM 创建是 upsert、删行幂等、宽限期挡住 in-flight）。
func (r *channelReconciler) reconcileOnce() (found, resolved, failed int, err error) {
	due, err := r.db.listDueChannelOutbox(time.Now().Add(-r.grace), r.batch)
	if err != nil {
		return 0, 0, 0, err
	}
	found = len(due)
	for _, o := range due {
		g, qErr := r.db.QueryWithGroupNo(o.GroupNo)
		if qErr != nil {
			r.Error("reconcile: query group failed", zap.Error(qErr), zap.String("groupNo", o.GroupNo))
			_ = r.db.markChannelOutboxAttempt(o.GroupNo, qErr.Error())
			failed++
			continue
		}
		if g == nil {
			// group 行已不存在：补偿删除成功但 outbox 删除失败，或群随后被解散。
			// 无需 IM 频道，清理陈旧 outbox 行即为收口。
			if delErr := r.db.deleteChannelOutbox(o.GroupNo); delErr != nil {
				r.Error("reconcile: delete stale outbox failed", zap.Error(delErr), zap.String("groupNo", o.GroupNo))
				failed++
				continue
			}
			resolved++
			continue
		}

		subs, sErr := r.subscribersOf(o.GroupNo)
		if sErr != nil {
			r.Error("reconcile: load subscribers failed", zap.Error(sErr), zap.String("groupNo", o.GroupNo))
			_ = r.db.markChannelOutboxAttempt(o.GroupNo, sErr.Error())
			failed++
			continue
		}

		if cErr := r.createChannel(&config.ChannelCreateReq{
			ChannelID:   o.GroupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: subs,
		}); cErr != nil {
			// IM 仍不可用：记一次尝试并保留 outbox 行，下一轮按退避重试（最终一致）。
			_ = r.db.markChannelOutboxAttempt(o.GroupNo, cErr.Error())
			r.Warn("reconcile: recreate IM channel failed, will retry",
				zap.Error(cErr), zap.String("groupNo", o.GroupNo), zap.Int("attempts", o.Attempts+1))
			failed++
			continue
		}

		if delErr := r.db.deleteChannelOutbox(o.GroupNo); delErr != nil {
			// 频道已建好但删行失败：下一轮会幂等重建频道(无副作用)再重试删行。
			r.Error("reconcile: delete outbox after channel recreate failed", zap.Error(delErr), zap.String("groupNo", o.GroupNo))
			failed++
			continue
		}
		r.Info("reconcile: recovered orphan group channel", zap.String("groupNo", o.GroupNo), zap.Int("subscribers", len(subs)))
		resolved++
	}
	if found > 0 {
		r.Info("group channel reconcile sweep done",
			zap.Int("found", found), zap.Int("resolved", resolved), zap.Int("failed", failed))
	}
	return found, resolved, failed, nil
}

// channelReconcileScheduler 定时驱动 channelReconciler（仿 opanalytics/backup 的 cron 调度）。
type channelReconcileScheduler struct {
	log.Log
	reconciler *channelReconciler
	cron       *cron.Cron
	entryID    cron.EntryID
	mu         sync.Mutex
	started    bool
}

func newChannelReconcileScheduler(ctx *config.Context) *channelReconcileScheduler {
	return &channelReconcileScheduler{
		Log:        log.NewTLog("GroupChannelReconcileScheduler"),
		reconciler: newChannelReconciler(ctx),
		cron:       cron.New(),
	}
}

// Start 启动调度器（幂等）。出错只返回 error，由调用方记日志，不 panic。
func (s *channelReconcileScheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	entryID, err := s.cron.AddFunc(reconcileCronExpr, s.runOnce)
	if err != nil {
		return err
	}
	s.entryID = entryID
	s.cron.Start()
	s.started = true
	s.Info("group channel reconcile scheduler started", zap.String("cron", reconcileCronExpr))
	return nil
}

func (s *channelReconcileScheduler) runOnce() {
	if _, _, _, err := s.reconciler.reconcileOnce(); err != nil {
		s.Error("group channel reconcile tick failed", zap.Error(err))
	}
}
