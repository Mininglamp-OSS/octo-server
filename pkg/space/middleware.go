package space

import (
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"go.uber.org/zap"
)

// abortL 用 i18n 错误信封中止请求。
//
// 此前这里用的是 c.AbortWithStatusJSON，直接违反仓库的 error-handling 规则，
// 且让挂载本中间件的每个端点的鉴权失败出口都游离在 OpenAPI 契约之外：响应体没有
// 错误码，客户端只能按 HTTP 状态码分支；文案是硬编码中文，不随 Accept-Language 变。
//
// 用 ResponseErrorLWithStatus 而不是 ResponseErrorL：后者把线路状态钉死成 400
// （D14 兼容），而本中间件既有的出口一直是 401/403/500，改成 400 会打断所有按
// 状态码分支的客户端——那是比它要修的问题更大的破坏。
//
// **这不是纯增量。** 信封是双形态的（renderer.go 同时输出 error{} 与 legacy 的
// msg/status），但 msg 的**值**由错误码的本地化文案重算，不是原样透传：
//
//	401  "请先登录"              -> "请先登录！"
//	403  "无权访问该 Space"      -> "你不是该空间成员。"
//	500  "校验 Space 成员身份失败" -> "服务器内部错误。"
//
// 500 那条最需要注意：ErrSpaceQueryFailed 带 Internal=true，渲染器会短路成通用
// 文案，具体原因不再出现在响应体里（按仓库约定，原因只进日志）。
// 任何按 msg 文本匹配的客户端都会受影响；按状态码或新的 error.code 分支的不会。
func abortL(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorLWithStatus(c, code, nil, nil)
	c.Abort()
}

// MembershipChecker 校验用户是否属于 Space 的函数签名。
type MembershipChecker func(spaceID string, uid string) (bool, error)

const cacheTTL = 60 * time.Second         // 正向缓存 60s
const negativeCacheTTL = 30 * time.Second  // 否定结果缓存 30s，新成员加入后快速生效

// MembershipCache 缓存成员身份校验结果的接口。
type MembershipCache interface {
	// Get 返回缓存的成员身份。found=false 表示缓存未命中。
	Get(spaceID, uid string) (isMember bool, found bool)
	// Set 写入缓存，ttl 为过期时间。
	Set(spaceID, uid string, isMember bool, ttl time.Duration)
}

// RedisMembershipCache 基于 Redis 的 MembershipCache 实现。
type RedisMembershipCache struct {
	redisConn *redis.Conn
}

// NewRedisMembershipCache 创建基于 Redis 的缓存。
func NewRedisMembershipCache(redisConn *redis.Conn) *RedisMembershipCache {
	return &RedisMembershipCache{redisConn: redisConn}
}

func redisCacheKey(spaceID, uid string) string {
	return fmt.Sprintf("space:member:%s:%s", spaceID, uid)
}

func (c *RedisMembershipCache) Get(spaceID, uid string) (bool, bool) {
	val, err := c.redisConn.GetString(redisCacheKey(spaceID, uid))
	if err != nil || val == "" {
		return false, false
	}
	return val == "1", true
}

func (c *RedisMembershipCache) Set(spaceID, uid string, isMember bool, ttl time.Duration) {
	val := "0"
	if isMember {
		val = "1"
	}
	_ = c.redisConn.SetAndExpire(redisCacheKey(spaceID, uid), val, ttl)
}

// InvalidateMembershipCache 删除指定用户在指定 Space 的成员缓存。
func InvalidateMembershipCache(redisConn *redis.Conn, spaceID, uid string) {
	_ = redisConn.Del(redisCacheKey(spaceID, uid))
}

// InMemoryMembershipCache 基于内存的 MembershipCache 实现，用于测试。
type InMemoryMembershipCache struct {
	entries map[string]inMemoryEntry
}

type inMemoryEntry struct {
	member   bool
	expireAt time.Time
}

func NewInMemoryMembershipCache() *InMemoryMembershipCache {
	return &InMemoryMembershipCache{entries: make(map[string]inMemoryEntry)}
}

func (c *InMemoryMembershipCache) Get(spaceID, uid string) (bool, bool) {
	key := fmt.Sprintf("%s:%s", spaceID, uid)
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expireAt) {
		return false, false
	}
	return entry.member, true
}

func (c *InMemoryMembershipCache) Set(spaceID, uid string, isMember bool, ttl time.Duration) {
	key := fmt.Sprintf("%s:%s", spaceID, uid)
	c.entries[key] = inMemoryEntry{member: isMember, expireAt: time.Now().Add(ttl)}
}

// Clear 清除所有缓存条目（测试用）。
func (c *InMemoryMembershipCache) Clear() {
	c.entries = make(map[string]inMemoryEntry)
}

// SpaceMiddleware 是 opt-in 中间件，route group 级别注入。
// 从请求提取 space_id（query param 优先，header X-Space-ID 其次），
// 无 space_id 则跳过，有则校验用户是否属于该 Space。
func SpaceMiddleware(ctx *config.Context) wkhttp.HandlerFunc {
	cache := NewRedisMembershipCache(ctx.GetRedisConn())
	return spaceMiddleware(func(spaceID, uid string) (bool, error) {
		return CheckMembership(ctx.DB(), spaceID, uid)
	}, cache)
}

func spaceMiddleware(check MembershipChecker, cache MembershipCache) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		spaceID := c.Query("space_id")
		if spaceID == "" {
			spaceID = c.GetHeader("X-Space-ID")
		}
		if spaceID == "" {
			c.Next()
			return
		}

		uid := c.GetLoginUID()
		if uid == "" {
			abortL(c, errcode.ErrSharedAuthRequired)
			return
		}

		// check cache
		if isMember, found := cache.Get(spaceID, uid); found {
			if !isMember {
				abortL(c, errcode.ErrSpaceNotMember)
				return
			}
			c.Set("space_id", spaceID)
			c.Next()
			return
		}

		// query DB
		isMember, err := check(spaceID, uid)
		if err != nil {
			// 必须在响应前记日志。ErrSpaceQueryFailed 是 Internal=true，渲染器会把
			// 消息短路成通用文案，原因不会出现在响应体里——这正是仓库
			// 「5xx ⟺ Internal=true，且响应前用 zap.Error 记录原因」约定的另一半。
			//
			// 迁移前响应体里至少还有「校验 Space 成员身份失败」这句，能把这个出口和
			// 其它 500 区分开；迁到信封后它与全站任何 Internal 错误逐字节相同。
			// 所以是这次改动欠下这行日志，不记的话 MySQL 抖动时 on-call 只能看到
			// 500 曲线，拿不到 space_id / uid / 具体错误。
			log.Error("校验 Space 成员身份失败",
				zap.Error(err), zap.String("space_id", spaceID), zap.String("uid", uid))
			abortL(c, errcode.ErrSpaceQueryFailed)
			return
		}

		ttl := cacheTTL
		if !isMember {
			ttl = negativeCacheTTL
		}
		cache.Set(spaceID, uid, isMember, ttl)

		if !isMember {
			abortL(c, errcode.ErrSpaceNotMember)
			return
		}

		c.Set("space_id", spaceID)
		c.Next()
	}
}

// GetSpaceID 从 gin context 读取 space_id。
func GetSpaceID(c *wkhttp.Context) string {
	if v, exists := c.Get("space_id"); exists {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
