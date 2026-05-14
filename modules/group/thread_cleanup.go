package group

import (
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"go.uber.org/zap"
)

// queryThreadShortIDsForCleanup 返回某群下所有需要在"成员被踢/退群"时摘除 IM 订阅的子区
// short_id（active + archived，排除 deleted）。
//
// Issue #27：旧实现把 thread JOIN thread_member 过滤，导致没主动 JoinThread 的成员（典型
// 就是 Bot）查不到子区 → IMRemoveSubscriber 不被调用 → Bot 被踢后仍订阅子区频道并通过
// WuKongIM WebSocket 持续收子区消息。子区频道的 IM 订阅本就是入群时按"父群成员"批量挂上
// 的（参考 addUsersToGroupThreads，WHERE status!=3），出群必须对称地按"父群所有非删除子区"
// 摘订阅。archived 子区可被 UnarchiveThread 重新激活，因此也必须摘除。
func queryThreadShortIDsForCleanup(ctx *config.Context, groupNo string) ([]string, error) {
	if groupNo == "" {
		return nil, nil
	}
	type row struct {
		ShortID string `db:"short_id"`
	}
	var rows []row
	_, err := ctx.DB().Select("short_id").
		From("thread").
		Where("group_no=? AND status!=3", groupNo).
		Load(&rows)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ShortID)
	}
	return out, nil
}

// removeUserFromGroupThreadsCleanup 是被踢/退群路径上"清理某用户在某群所有子区的 thread_member
// 记录、IM 订阅和置顶"的统一入口。原本 (*Group).removeUserFromGroupThreads 与
// (*Service).removeUserFromGroupThreads 是两份逐字重复的实现且各自带 Issue #27 的同型 bug；
// 这里合并到一处，避免下次再"修一处漏一处"。
//
// 调用方负责自己的日志 zap 字段上下文，这里的错误仅写 ctx 默认 logger（与原行为一致：失败
// 只记日志、不中断）。logger 由调用方传入以保留原有 module 标签。
func removeUserFromGroupThreadsCleanup(ctx *config.Context, logger log.Log, groupNo, uid, spaceID string) {
	if groupNo == "" || uid == "" {
		return
	}
	shortIDs, err := queryThreadShortIDsForCleanup(ctx, groupNo)
	if err != nil {
		logger.Error("查询群子区失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", uid))
		return
	}
	if len(shortIDs) == 0 {
		return
	}

	// best-effort 删除 thread_member 行：DELETE 按 uid 过滤，没有匹配行也是 0 rows affected。
	// 即使删除失败也要继续摘 IM 订阅 —— 不能让 DB 异常导致订阅泄漏。
	if _, err := ctx.DB().DeleteFrom("thread_member").
		Where("uid=? AND thread_id IN (SELECT id FROM thread WHERE group_no=?)", uid, groupNo).
		Exec(); err != nil {
		logger.Error("删除子区成员记录失败", zap.Error(err), zap.String("groupNo", groupNo), zap.String("uid", uid))
	}

	for _, shortID := range shortIDs {
		// 子区 channelID 格式: {groupNo}____{shortID} (与 thread.BuildChannelID 一致)
		channelID := groupNo + "____" + shortID
		if rmErr := ctx.IMRemoveSubscriber(&config.SubscriberRemoveReq{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Subscribers: []string{uid},
		}); rmErr != nil {
			logger.Error("移除子区IM订阅者失败", zap.Error(rmErr), zap.String("channelID", channelID), zap.String("uid", uid))
		}
		user.RemovePinnedForUserInSpace(uid, spaceID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	}
}
