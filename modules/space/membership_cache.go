package space

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

// revokeMembershipCache 清除给定 uid 在该 Space 的成员判定缓存。
//
// 每条把成员移出 Space 的写路径都要在 **DB 提交之后** 调用它。SpaceMiddleware
// 把正向判定缓存 60s（pkg/space/middleware.go cacheTTL），不清除就等于撤权最长
// 延迟 60 秒生效。GET /v1/space/{space_id}/directory 走这个中间件，返回的是整份
// 成员名册（uid + 展示名 + 角色 + 全部 bot），所以这段窗口里被移除的人仍能拿到
// 完整 PII。它取代的 listMembers 是逐请求查库、撤权即时生效的，因此对该接口而言
// 这不是继承来的旧问题，而是迁移引入的实时性回退——必须一起补上。
//
// 顺序很重要：先提交 DB 再清缓存。反过来的话，清除与提交之间任何一次请求都会
// 重新把「是成员」写回缓存，撤权直接失效。
//
// 失败只记 WARN，不改变 HTTP 结果：成员行已经落库为已移除，DB 才是权威；Redis
// 清不掉只是让撤权退回到 TTL 到期生效（≤60s），把已经成功的移除操作报成失败对
// 调用方更糟。日志是发现这种静默降级的唯一入口，所以必须记，且带上 uid。
func revokeMembershipCache(ctx *config.Context, lg log.Log, spaceID string, uids ...string) {
	redisConn := ctx.GetRedisConn()
	if redisConn == nil {
		return
	}
	var failed []string
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		if err := spacepkg.InvalidateMembershipCache(redisConn, spaceID, uid); err != nil {
			failed = append(failed, uid)
		}
	}
	if len(failed) > 0 {
		lg.Warn("清除 Space 成员缓存失败，撤权将延迟到缓存过期（≤60s）后生效",
			zap.String("spaceId", spaceID), zap.Strings("uids", failed))
	}
}

// revokeMembershipCacheForSpace 清除整个 Space 的成员判定缓存，用于解散。
//
// 解散只翻 space.status（用户侧）或同时翻成员行（管理端强制解散），两种情况下
// CheckMembership 都会立即返回 false —— 它的 SQL 带 `INNER JOIN space s ON
// s.status = 1`。也就是说解散同样是一次撤权，只不过一次性作用于全部成员。
//
// 取 uid 时刻意不加 status 过滤，且在提交之后再查：这样刚好在解散事务窗口内加入
// 的成员（其成员行已存在、缓存可能已被写成正向）也会被覆盖到。对已经没有缓存条目
// 的历史成员多发一次 Del 是无害的（幂等且廉价），漏掉一个正向条目才是安全问题。
//
// 没有用 KEYS space:member:{id}:* 来枚举：KEYS 会阻塞整个 Redis 实例扫描全库，
// 在生产上是禁用级别的操作；按成员行枚举的成本由 Space 大小封顶，而解散本身是
// 低频操作。
func revokeMembershipCacheForSpace(ctx *config.Context, lg log.Log, db *DB, spaceID string) {
	uids, err := db.queryAllMemberUIDs(spaceID)
	if err != nil {
		lg.Warn("查询 Space 成员失败，无法清除成员缓存，撤权将延迟到缓存过期（≤60s）后生效",
			zap.String("spaceId", spaceID), zap.Error(err))
		return
	}
	revokeMembershipCache(ctx, lg, spaceID, uids...)
}
