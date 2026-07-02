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

// reconcileStore ReconcileWorker 对持久层的最小依赖，便于单测注入 stub（无需真实 DB）。
type reconcileStore interface {
	// QueryUnsyncedChannelGroups 返回 channel_synced=0 且 created_at<cutoff 的群编号。
	QueryUnsyncedChannelGroups(cutoff time.Time, limit int) ([]string, error)
	// SubscribableMemberUIDs 返回某群「可订阅」成员 uid（is_deleted=0 且 status=normal）。
	SubscribableMemberUIDs(groupNo string) ([]string, error)
	// SetChannelSynced 把某群 channel_synced 置为指定值。
	SetChannelSynced(groupNo string, synced int) error
}

// channelCreator 抽出 IM 频道创建能力（生产实现是 *config.Context），便于单测 mock。
// IMCreateOrUpdateChannel 语义为「创建或更新」，天然幂等——重复补建已存在的频道安全。
type channelCreator interface {
	IMCreateOrUpdateChannel(req *config.ChannelCreateReq) error
}

// dbReconcileStore 把 *group.DB 适配到 reconcileStore（生产实现）。
type dbReconcileStore struct{ db *DB }

func (s dbReconcileStore) QueryUnsyncedChannelGroups(cutoff time.Time, limit int) ([]string, error) {
	return s.db.QueryUnsyncedChannelGroups(cutoff, limit)
}

func (s dbReconcileStore) SubscribableMemberUIDs(groupNo string) ([]string, error) {
	return s.db.querySubscribableMemberUIDsWithGroupNo(groupNo)
}

func (s dbReconcileStore) SetChannelSynced(groupNo string, synced int) error {
	return s.db.SetChannelSynced(groupNo, synced)
}

// ReconcileWorker 周期性扫描 channel_synced=0 的孤儿群，幂等补建其 WuKongIM 频道，
// 成功后翻 channel_synced=1（#394）。它是 CreateGroup 补偿删除失败 / 进程崩溃窗口的
// 兜底：把「可查询但无 backing 频道」的永久孤儿收敛为最终一致。
type ReconcileWorker struct {
	cfg   ReconcileConfig
	store reconcileStore
	im    channelCreator
	now   func() time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
	log.Log
}

// NewReconcileWorker 构造生产 worker：从 *config.Context 派生 DB store 与 IM 频道创建能力。
func NewReconcileWorker(ctx *config.Context, cfg ReconcileConfig) *ReconcileWorker {
	return &ReconcileWorker{
		cfg:   cfg,
		store: dbReconcileStore{db: NewDB(ctx)},
		im:    ctx,
		now:   time.Now,
		Log:   log.NewTLog("GroupChannelReconcileWorker"),
	}
}

// Start 启动后台 ticker。Enabled=false / Interval<=0 时不启动新 goroutine，但仍先停掉
// 可能存在的旧 goroutine（防未来热更留孤儿 ticker）。重复调用幂等：先 stop 再 start。
func (w *ReconcileWorker) Start(ctx context.Context) {
	if w.cancel != nil {
		w.cancel()
		w.wg.Wait()
		w.cancel = nil
	}
	if !w.cfg.Enabled || w.cfg.Interval <= 0 {
		w.Info("group channel reconcile worker disabled",
			zap.Bool("enabled", w.cfg.Enabled),
			zap.Duration("interval", w.cfg.Interval))
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		w.Info("group channel reconcile worker started",
			zap.Duration("interval", w.cfg.Interval),
			zap.Duration("grace", w.cfg.Grace),
			zap.Int("batch_size", w.cfg.BatchSize))
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				resolved, err := w.RunOnce(rctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					w.Error("group channel reconcile run failed", zap.Error(err))
					continue
				}
				if resolved > 0 {
					w.Info("group channel reconcile run", zap.Int("resolved", resolved))
				}
			}
		}
	}()
}

// Stop 通知 worker 退出并等待当前 RunOnce 跑完。
func (w *ReconcileWorker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

// RunOnce 执行一轮 reconcile：拉一批孤儿群 → 逐个幂等补建频道 → 成功翻 channel_synced=1。
// 返回本轮成功修复的孤儿数。
//
// 安全保护：
//   - BatchSize<=0 视为禁用，直接返回 (0,nil)；Grace<=0 时回退默认宽限，避免和进行中的
//     建群事务竞争（cutoff = now-grace）。
//   - 单个孤儿处理失败（IM 创建 / 翻标记）只 log+metric，不中断整批——下个 tick 自然重试。
//   - ctx 取消时立即停止派发，返回 ctx.Err() 让上层区分「正常停机」vs「异常」。
func (w *ReconcileWorker) RunOnce(ctx context.Context) (int, error) {
	batch := w.cfg.BatchSize
	if batch <= 0 {
		return 0, nil
	}
	grace := w.cfg.Grace
	if grace <= 0 {
		grace = defaultReconcileGrace
	}
	cutoff := w.now().Add(-grace)

	groupNos, err := w.store.QueryUnsyncedChannelGroups(cutoff, batch)
	if err != nil {
		metricReconcileTickTotal.WithLabelValues("error").Inc()
		return 0, err
	}
	metricReconcileTickTotal.WithLabelValues("ran").Inc()

	var resolved int
	for _, groupNo := range groupNos {
		if err := ctx.Err(); err != nil {
			return resolved, err
		}
		metricReconcileOrphanTotal.WithLabelValues("detected").Inc()
		if w.reconcileOne(groupNo) {
			resolved++
		}
	}
	return resolved, nil
}

// reconcileOne 修复单个孤儿群：拉当前可订阅成员 → 幂等补建 IM 频道 → 翻 channel_synced=1。
// 返回是否成功修复（频道已确认在且标记已翻 1）。
//
// 关键点：
//   - 订阅者取自 group_member 权威列表（与 IMDatasource.Subscribers 回调同源），WuKongIM
//     后续重载订阅也会以它为准，故此处传空列表也不会丢成员——但仍显式带上，缩短可见窗口。
//   - IMCreateOrUpdateChannel 幂等：即便频道其实已存在（进程在翻标记前崩溃的场景），
//     重复调用只是刷新，安全。
func (w *ReconcileWorker) reconcileOne(groupNo string) bool {
	subscribers, err := w.store.SubscribableMemberUIDs(groupNo)
	if err != nil {
		// 拉成员失败不阻断补建：WuKongIM 会从 Subscribers 数据源回填，仍以空列表补建频道，
		// 至少消除「无 backing 频道」这一硬性缺陷；成员列表下个订阅重载自愈。
		w.Warn("reconcile: query subscribers failed, creating channel with empty list",
			zap.Error(err), zap.String("groupNo", groupNo))
		subscribers = nil
	}
	if err := w.im.IMCreateOrUpdateChannel(&config.ChannelCreateReq{
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: subscribers,
	}); err != nil {
		metricReconcileOrphanTotal.WithLabelValues("im_failed").Inc()
		w.Warn("reconcile: create IM channel failed, will retry next tick",
			zap.Error(err), zap.String("groupNo", groupNo))
		return false
	}
	if err := w.store.SetChannelSynced(groupNo, 1); err != nil {
		// 频道已补建成功，但标记没翻——下个 tick 会再次命中该行、确认频道已在后补翻 1。
		metricReconcileOrphanTotal.WithLabelValues("mark_failed").Inc()
		w.Warn("reconcile: channel created but mark channel_synced=1 failed, will retry next tick",
			zap.Error(err), zap.String("groupNo", groupNo))
		return false
	}
	metricReconcileOrphanTotal.WithLabelValues("resolved").Inc()
	w.Info("reconcile: orphan group channel recreated", zap.String("groupNo", groupNo))
	return true
}
