package space

import (
	"sync"

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
// 但这个顺序**只是把窗口收窄，并没有消灭它**：一个并发请求可能在移除提交之前就
// 读完了库（拿到 is_member=true），却在这里的 DEL 之后才把结果写进缓存
// （pkg/space/middleware.go 里 check() 与 cache.Set() 之间的那段）。撞上的话，
// 被移除的人会重新获得完整的 60s。窗口是次毫秒级的，需要注入延迟才能稳定复现，
// 但它确实存在，不该被 RevokesImmediately 这样的测试名盖过去。
// 彻底关掉需要墓碑键或延迟二次删除，那是本任务范围之外的独立改动。
//
// 失败只记 WARN，不改变 HTTP 结果：成员行已经落库为已移除，DB 才是权威；Redis
// 清不掉只是让撤权退回到 TTL 到期生效（≤60s），把已经成功的移除操作报成失败对
// 调用方更糟。日志是发现这种静默降级的唯一入口，所以必须记，且带上 uid。
func revokeMembershipCache(ctx *config.Context, lg log.Log, spaceID string, uids ...string) {
	redisConn := ctx.GetRedisConn()
	var failed []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	// 并发上限。octo-lib 的 redis.Conn 只暴露单键 Del，没有 pipeline，所以整份
	// Space 失效（解散 / 封禁）会是每个 uid 一次往返。实测 6000 个成员在**回环**
	// Redis 上串行要 504ms，按生产 0.5–1ms RTT 换算是 3–6 秒——而这段时间挂在
	// DELETE /v1/space/{id} 的请求线程上。放到后台 goroutine 会重新打开撤权窗口，
	// 所以改成有界并发：go-redis 的 client 本身是并发安全的，16 路把最坏情况压回
	// 几十毫秒，同时不会把连接池打满。
	sem := make(chan struct{}, 16)
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := spacepkg.InvalidateMembershipCache(redisConn, spaceID, uid); err != nil {
				mu.Lock()
				failed = append(failed, uid)
				mu.Unlock()
			}
		}(uid)
	}
	wg.Wait()
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
	// 注意成本：queryAllMemberUIDs 不带 status 过滤也不带 LIMIT，返回的是该 Space
	// **历史上**的全部成员行。这是刻意的（见上），但也意味着条数按历史成员规模增长，
	// 而不是当前成员规模。失效本身已改为有界并发，见 revokeMembershipCache。
	uids, err := db.queryAllMemberUIDs(spaceID)
	if err != nil {
		lg.Warn("查询 Space 成员失败，无法清除成员缓存，撤权将延迟到缓存过期（≤60s）后生效",
			zap.String("spaceId", spaceID), zap.Error(err))
		return
	}
	revokeMembershipCache(ctx, lg, spaceID, uids...)
}
