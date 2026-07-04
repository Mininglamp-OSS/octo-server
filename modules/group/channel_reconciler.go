package group

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

// reconcileDB 抽出 ChannelReconciler 需要的最小 DB 接口，便于单测 mock。
type reconcileDB interface {
	QueryUnsyncedGroupNos(before time.Time, limit int) ([]string, error)
	QueryMemberUIDsWithGroupNo(groupNo string) ([]string, error)
	MarkChannelSynced(groupNo string) error
	DeleteOrphanGroup(groupNo string) error
}

// reconcileIM 抽出重建 IM 频道所需的最小接口（*config.Context 实现之），便于单测 mock。
type reconcileIM interface {
	IMCreateOrUpdateChannel(req *config.ChannelCreateReq) error
}

// ChannelReconciler 周期性扫描 channel_synced=0 的群，收口 CreateGroup「commit 后建频道」
// 留下的孤儿群：有成员 → 幂等重建 IM 频道并标记 synced；无成员（补偿删除已删成员但群行
// 残留）→ 硬删除孤儿群行。实现最终一致，闭合 #394 的孤儿窗口。
type ChannelReconciler struct {
	cfg ChannelReconcileConfig
	db  reconcileDB
	im  reconcileIM
	now func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	log.Log
}

// NewChannelReconciler 构造 worker。生产路径用 group.NewDB 和 config.Context 注入。
func NewChannelReconciler(ctx *config.Context, cfg ChannelReconcileConfig) *ChannelReconciler {
	return &ChannelReconciler{
		cfg: cfg,
		db:  NewDB(ctx),
		im:  ctx,
		now: time.Now,
		Log: log.NewTLog("GroupChannelReconciler"),
	}
}

// Start 启动后台 ticker。Enabled=false 或参数非法时不启动新 goroutine，但仍先停掉可能
// 存在的旧 goroutine——避免未来热更新留下孤儿 ticker。重复调用幂等。
func (w *ChannelReconciler) Start(ctx context.Context) {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
		w.cancel = nil
	}
	if !w.cfg.Enabled || w.cfg.Interval <= 0 || w.cfg.BatchSize <= 0 {
		w.Info("group channel reconciler disabled",
			zap.Bool("enabled", w.cfg.Enabled),
			zap.Duration("interval", w.cfg.Interval),
			zap.Int("batch_size", w.cfg.BatchSize))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		w.Info("group channel reconciler started",
			zap.Duration("interval", w.cfg.Interval),
			zap.Duration("grace", w.cfg.Grace),
			zap.Int("batch_size", w.cfg.BatchSize))
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				recreated, deleted, err := w.RunOnce(rctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					w.Error("group channel reconcile run failed", zap.Error(err))
					continue
				}
				if recreated > 0 || deleted > 0 {
					w.Info("group channel reconcile run",
						zap.Int("recreated", recreated),
						zap.Int("deleted", deleted))
				}
			}
		}
	}()
}

// Stop 通知 worker 退出并等待当前 RunOnce 跑完。
func (w *ChannelReconciler) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// RunOnce 执行一轮对账：取一批 channel_synced=0 且早于宽限期的群，逐个收口。
// 返回本轮 (重建频道数, 硬删除孤儿数, err)。单个群失败只记日志、不中断整批——
// 下一轮 tick 会重试（IMCreateOrUpdateChannel 是 upsert 幂等，重跑安全）。
func (w *ChannelReconciler) RunOnce(ctx context.Context) (int, int, error) {
	if w.cfg.BatchSize <= 0 {
		return 0, 0, nil
	}
	before := w.now().Add(-w.cfg.Grace)
	groupNos, err := w.db.QueryUnsyncedGroupNos(before, w.cfg.BatchSize)
	if err != nil {
		return 0, 0, err
	}
	var recreated, deleted int
	for _, groupNo := range groupNos {
		if err := ctx.Err(); err != nil {
			return recreated, deleted, err
		}
		uids, err := w.db.QueryMemberUIDsWithGroupNo(groupNo)
		if err != nil {
			w.Error("reconcile: query members failed", zap.String("groupNo", groupNo), zap.Error(err))
			continue
		}
		if len(uids) == 0 {
			// 补偿删除部分成功：成员已删、群行残留 → 群无有效成员、不可恢复，硬删除孤儿。
			if err := w.db.DeleteOrphanGroup(groupNo); err != nil {
				w.Error("reconcile: delete memberless orphan failed", zap.String("groupNo", groupNo), zap.Error(err))
				continue
			}
			deleted++
			w.Info("reconcile: hard-deleted memberless orphan group", zap.String("groupNo", groupNo))
			continue
		}
		// 有成员 → 幂等重建 IM 频道，成功后标记 synced。失败留到下轮重试。
		if err := w.im.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
			ChannelID:   groupNo,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Subscribers: uids,
		}); err != nil {
			w.Error("reconcile: recreate IM channel failed, will retry next round",
				zap.String("groupNo", groupNo), zap.Error(err))
			continue
		}
		if err := w.db.MarkChannelSynced(groupNo); err != nil {
			// 频道已建成、仅标记失败：下轮再跑一次（IMCreate 幂等）即可收敛，无害。
			w.Error("reconcile: mark synced failed after channel recreate",
				zap.String("groupNo", groupNo), zap.Error(err))
			continue
		}
		recreated++
		w.Info("reconcile: recreated IM channel for orphan group",
			zap.String("groupNo", groupNo), zap.Int("subscribers", len(uids)))
	}
	return recreated, deleted, nil
}
