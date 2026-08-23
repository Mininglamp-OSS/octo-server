package space

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// imPendingRunning 进程内重入保护，同 removalCleanupRunning：定时器「先安排下一次、
// 再执行本次」，不会等上一轮跑完。
var imPendingRunning atomic.Bool

func nowUTC() time.Time { return time.Now().UTC() }

// 退订待办：让 IMRemoveSubscriber 的失败活下来。
//
// 问题（Mininglamp-OSS/octo-server#797）：删掉 group_member 行之后调 IMRemoveSubscriber，
// 失败只打日志。人在业务库里已不是成员，在 WuKongIM 里还是订阅者。实测这种「泄漏态」
// 与正常群成员**完全无差别** —— 照发照收；而且四条自愈路径全断（重跑工单是空转、
// broker 不重载、用户看不到这个群、管理员看不到这个人），所以泄漏是永久的。
//
// 用法分两种形状，取决于调用点有没有事务可用：
//
//	有事务（RemoveGroupMembers、groupExit 的 bot 级联）：
//	    EnqueueIMUnsubscribeTx(tx, ...)   // 与成员行删除同事务提交
//	    tx.Commit()
//	    AttemptIMUnsubscribe(ctx, ...)    // 提交后立刻试一次，成功即删待办
//
//	无事务（thread_cleanup、blacklist、botfather 删 bot）：
//	    EnqueueIMUnsubscribe(ctx.DB(), ...)
//	    AttemptIMUnsubscribe(ctx, ...)
//
// 两者行为一致（总是先记录、再尝试、成功删行），差别只在 INSERT 有没有原子性——
// 那是既有代码形状决定的，不是策略选择。给无事务的三处硬加事务，改动量远超本任务。

// EnqueueIMUnsubscribeTx 在调用方事务内写出退订待办。
//
// 必须在**删除成员行的同一个事务**里调用。这样「成员已删、待办已记」是一个原子事实：
// 进程若在提交之后、IM 调用返回之前被杀（滚动发布时的 pod 驱逐正是这个形状，而 IM 调用
// 慢恰恰发生在 broker 有压力时），待办仍在，worker 会把它排掉。
func EnqueueIMUnsubscribeTx(tx *dbr.Tx, channelID string, channelType uint8, uids []string) error {
	return enqueueIMUnsubscribe(tx, channelID, channelType, uids)
}

// EnqueueIMUnsubscribe 无事务版本，供没有事务可用的调用点使用。
// 必须在真正发起 IM 调用**之前**调用。
func EnqueueIMUnsubscribe(session *dbr.Session, channelID string, channelType uint8, uids []string) error {
	return enqueueIMUnsubscribe(session, channelID, channelType, uids)
}

// AttemptIMUnsubscribe 立刻尝试一次退订；成功就删掉对应待办，失败就把待办留给 worker。
//
// 返回 IM 调用的错误供调用方记日志。调用方**不应**据此让请求失败：待办已经落库，
// 收敛由 worker 负责，把一次后台可修复的抖动变成用户可见的 500 没有意义。
func AttemptIMUnsubscribe(ctx *config.Context, channelID string, channelType uint8, uids []string) error {
	if channelID == "" || len(uids) == 0 {
		return nil
	}
	if err := ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
		ChannelID:   channelID,
		ChannelType: channelType,
		Subscribers: uids,
	}); err != nil {
		return err
	}
	// 删不掉不影响正确性：worker 会再退订一次（实测幂等：对已退订者、未知用户、
	// 未知频道都返回 200），然后删掉这行。
	return deleteIMPendingByTarget(ctx.DB(), channelID, channelType, uids)
}

// processIMPendingRemovals 排掉一批退订待办。
//
// 与 processMemberRemovalCleanups 分开而不是并进去：五个泄漏点里只有两个来自 Space
// 成员移除，拉黑 / 退群 bot 级联 / 删 bot 都没有对应的成员移除工单。共用调度与退避
// 曲线，但各自一张表。
func (s *Space) processIMPendingRemovals() {
	if !imPendingRunning.CompareAndSwap(false, true) {
		return
	}
	defer imPendingRunning.Store(false)

	for i := 0; i < removalCleanupBatchSize; i++ {
		owner := newRemovalClaimOwner()
		row, err := s.db.claimIMPendingRemoval(owner, nowUTC())
		if err != nil {
			s.Warn("认领退订待办失败", zap.Error(err))
			return
		}
		if row == nil {
			return
		}
		if imErr := s.ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   row.ChannelID,
			ChannelType: row.ChannelType,
			Subscribers: []string{row.UID},
		}); imErr != nil {
			s.handleIMPendingFailure(row, owner, imErr)
			continue
		}
		if delErr := s.db.deleteIMPendingByID(row.ID, owner); delErr != nil {
			// 退订本身已经成功，隔离边界已经守住；删行失败只会让这条待办再跑一次
			// （幂等），下一轮删掉。
			s.Warn("删除已完成的退订待办失败", zap.Uint64("id", row.ID), zap.Error(delErr))
		}
	}
}

// handleIMPendingFailure 失败分流：预算耗尽置 abandoned，否则按退避重排。
func (s *Space) handleIMPendingFailure(row *imPendingRow, owner string, cause error) {
	if row.Attempts >= removalCleanupMaxAttempts {
		s.Error("退订待办重试耗尽，置为 abandoned；被移除者仍保有该频道的收发权限，需人工介入",
			zap.Uint64("id", row.ID), zap.String("channelId", row.ChannelID),
			zap.String("uid", row.UID), zap.Uint32("attempts", row.Attempts), zap.Error(cause))
		if err := s.db.abandonIMPending(row.ID, owner, fmt.Sprintf("im unsubscribe: %v", cause)); err != nil {
			s.Warn("置退订待办为 abandoned 失败", zap.Uint64("id", row.ID), zap.Error(err))
		}
		return
	}
	s.Warn("退订失败，稍后重试",
		zap.Uint64("id", row.ID), zap.String("channelId", row.ChannelID),
		zap.String("uid", row.UID), zap.Uint32("attempts", row.Attempts), zap.Error(cause))
	if err := s.db.releaseIMPending(row.ID, owner, row.Attempts,
		fmt.Sprintf("im unsubscribe: %v", cause)); err != nil {
		s.Warn("释放退订待办失败", zap.Uint64("id", row.ID), zap.Error(err))
	}
}

// sweepExhaustedIMPending 进程外扫描，与成员移除工单那条同理。
func (s *Space) sweepExhaustedIMPending() {
	abandoned, err := s.db.abandonExhaustedIMPending(nowUTC(), removalSweepLimit)
	if err != nil {
		s.Warn("扫描重试耗尽的退订待办失败", zap.Error(err))
		return
	}
	if abandoned > 0 {
		s.Error("退订待办重试耗尽，已置为 abandoned；无自动重驱动，需人工介入",
			zap.Int64("abandoned", abandoned))
	}
}
