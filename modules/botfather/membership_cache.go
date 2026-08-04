package botfather

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// revokeBotMembershipCache 清除 bot 在其所有 Space 上的成员判定缓存。
//
// 删除 bot 的两条路径（/deletebot 命令与 DELETE /v1/user/bots/{id}）都会执行
// `UPDATE space_member SET status=0 WHERE uid=?`，把 bot 一次性移出全部 Space。
// 那是一次撤权，和把真人踢出空间同类：SpaceMiddleware 会把正向判定缓存 60s
// （pkg/space/middleware.go cacheTTL），不清就意味着已删除的 bot 在这段窗口里
// 仍可读它原先所在 Space 的数据。
//
// 在 UPDATE 之后调用即可：SpaceIDsForMember 刻意不按 status 过滤，所以先改后清
// 依然能枚举到全部 Space（也因此不受这两条路径「UPDATE 失败只记日志、不中断」
// 的影响——即使写失败，清一遍缓存也无害）。
//
// 失败只记 WARN：bot 行已经落库为已移除，DB 是权威；Redis 清不掉只是让撤权退回
// 到 TTL 到期生效。删除流程本身也不因缓存清理失败而回滚。
func revokeBotMembershipCache(ctx *config.Context, lg log.Log, botID string) {
	spaceIDs, err := spacepkg.SpaceIDsForMember(ctx.DB(), botID)
	if err != nil {
		lg.Warn("查询Bot所属Space失败，无法清除成员缓存，撤权将延迟到缓存过期（≤60s）后生效",
			zap.String("botID", botID), zap.Error(err))
		return
	}
	redisConn := ctx.GetRedisConn()
	if redisConn == nil {
		return
	}
	var failed []string
	for _, spaceID := range spaceIDs {
		if spaceID == "" {
			continue
		}
		if err := spacepkg.InvalidateMembershipCache(redisConn, spaceID, botID); err != nil {
			failed = append(failed, spaceID)
		}
	}
	if len(failed) > 0 {
		lg.Warn("清除Bot的Space成员缓存失败，撤权将延迟到缓存过期（≤60s）后生效",
			zap.String("botID", botID), zap.Strings("spaceIds", failed))
	}
}
