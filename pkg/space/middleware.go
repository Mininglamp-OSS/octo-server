package space

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/redis"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

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
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "请先登录"})
			return
		}

		// check cache
		if isMember, found := cache.Get(spaceID, uid); found {
			if !isMember {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权访问该 Space"})
				return
			}
			SetSpaceID(c, spaceID)
			c.Next()
			return
		}

		// query DB
		isMember, err := check(spaceID, uid)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"msg": "校验 Space 成员身份失败"})
			return
		}

		ttl := cacheTTL
		if !isMember {
			ttl = negativeCacheTTL
		}
		cache.Set(spaceID, uid, isMember, ttl)

		if !isMember {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"msg": "无权访问该 Space"})
			return
		}

		SetSpaceID(c, spaceID)
		c.Next()
	}
}

// ctxKeySpaceID is the gin context key carrying the request's verified Space.
// SetSpaceID and GetSpaceID are its only accessors.
const ctxKeySpaceID = "space_id"

// SetSpaceID marks spaceID as the request's verified Space, so handlers reading
// GetSpaceID apply their Space isolation. The caller owns the verification:
// SpaceMiddleware checks membership itself, and a non-session authentication
// tree publishes the tenant its credential was issued against.
func SetSpaceID(c *wkhttp.Context, spaceID string) {
	c.Set(ctxKeySpaceID, spaceID)
}

// GetSpaceID 从 gin context 读取 space_id。
func GetSpaceID(c *wkhttp.Context) string {
	if v, exists := c.Get(ctxKeySpaceID); exists {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
