package space

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// wrapWK converts a wkhttp.HandlerFunc into a gin.HandlerFunc for testing.
func wrapWK(h wkhttp.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		wc := &wkhttp.Context{Context: c}
		h(wc)
	}
}

var testCache *InMemoryMembershipCache

// setupRouter creates a test router with the space middleware and a simple 200 handler.
func setupRouter(checker MembershipChecker) *gin.Engine {
	testCache = NewInMemoryMembershipCache()
	r := gin.New()
	mw := spaceMiddleware(checker, testCache)
	r.Use(func(c *gin.Context) {
		// simulate auth: set uid so GetLoginUID works
		c.Set("uid", "testuser")
		c.Set("name", "Test")
		c.Next()
	})
	r.Use(wrapWK(mw))
	r.GET("/test", func(c *gin.Context) {
		spaceID, _ := c.Get("space_id")
		c.JSON(http.StatusOK, gin.H{"space_id": spaceID})
	})
	return r
}

func TestSpaceMiddleware_NoSpaceID_PassThrough(t *testing.T) {
	called := false
	checker := func(spaceID, uid string) (bool, error) {
		called = true
		return false, nil
	}
	r := setupRouter(checker)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, called, "checker should not be called when no space_id")
}

func TestSpaceMiddleware_NotMember_403(t *testing.T) {
	checker := func(spaceID, uid string) (bool, error) {
		return false, nil
	}
	r := setupRouter(checker)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test?space_id=sp1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestSpaceMiddleware_IsMember_PassWithContext(t *testing.T) {
	checker := func(spaceID, uid string) (bool, error) {
		return true, nil
	}
	r := setupRouter(checker)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test?space_id=sp1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "sp1")
}

func TestSpaceMiddleware_Header_SpaceID(t *testing.T) {
	checker := func(spaceID, uid string) (bool, error) {
		assert.Equal(t, "sp-header", spaceID)
		return true, nil
	}
	r := setupRouter(checker)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Space-ID", "sp-header")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestSpaceMiddleware_CacheHit(t *testing.T) {
	callCount := 0
	checker := func(spaceID, uid string) (bool, error) {
		callCount++
		return true, nil
	}
	r := setupRouter(checker)

	// first request — cache miss
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test?space_id=sp1", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, callCount)

	// second request — cache hit
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/test?space_id=sp1", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, callCount, "checker should not be called again due to cache")
}

// stubCacheStore 让 DEL / SET 可控地失败，用来验证 InvalidateMembershipCache
// 在删不掉时的兜底行为。抽 seam 的成例见 botfather 的 enforceKeySpaceWithChecker。
type stubCacheStore struct {
	delErr   error
	setErr   error
	delCalls []string
	setCalls []stubSetCall
}

type stubSetCall struct {
	key   string
	value interface{}
	ttl   time.Duration
}

func (s *stubCacheStore) Del(key string) error {
	s.delCalls = append(s.delCalls, key)
	return s.delErr
}

func (s *stubCacheStore) SetAndExpire(key string, value interface{}, expire time.Duration) error {
	s.setCalls = append(s.setCalls, stubSetCall{key: key, value: value, ttl: expire})
	return s.setErr
}

// TestInvalidateMembershipCacheDeletesOnHappyPath 正常路径只 DEL，不写任何东西。
func TestInvalidateMembershipCacheDeletesOnHappyPath(t *testing.T) {
	store := &stubCacheStore{}
	if err := invalidateMembershipCacheIn(store, "sp1", "u1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.delCalls) != 1 || store.delCalls[0] != redisCacheKey("sp1", "u1") {
		t.Errorf("expected one DEL on the membership key, got %v", store.delCalls)
	}
	if len(store.setCalls) != 0 {
		t.Errorf("happy path must not write anything, got %v", store.setCalls)
	}
}

// TestInvalidateMembershipCacheFallsBackToNegativeEntry 这是本条修复的核心。
//
// 整个 Redis 挂掉反而是安全的：中间件的 Get 未命中，会回落到查库。危险的是**单独**
// DEL 失败——正向条目 "1" 会活满它的 60s TTL，SpaceMiddleware 继续放行这个已经被
// 移除的人，而 handler 早已提交并返回 200，日志里一个字都没有。重新发起一次移除也
// 救不回来：removeMemberLocked 返回 ok=false，afterMembersRemoved 根本不会为这个
// uid 再跑一次。
//
// 所以删不掉时要主动写一条否定缓存把它盖掉，而不是干等 TTL 过期。
func TestInvalidateMembershipCacheFallsBackToNegativeEntry(t *testing.T) {
	store := &stubCacheStore{delErr: errors.New("redis del timeout")}

	err := invalidateMembershipCacheIn(store, "sp1", "u1")
	if err == nil {
		t.Fatal("DEL 失败必须上报，调用方要能记日志")
	}
	if len(store.setCalls) != 1 {
		t.Fatalf("DEL 失败后必须写一条否定缓存兜底，got %v", store.setCalls)
	}
	got := store.setCalls[0]
	if got.key != redisCacheKey("sp1", "u1") {
		t.Errorf("否定缓存写错了 key: %s", got.key)
	}
	if got.value != "0" {
		t.Errorf("兜底写入必须是否定值 \"0\"，got %v", got.value)
	}
	if got.ttl != negativeCacheTTL {
		t.Errorf("兜底 TTL 应为 negativeCacheTTL，got %v", got.ttl)
	}
	// 调用方靠这个哨兵把「边界守住了」与「边界没守住」分开记日志。丢了它，
	// 兜底成功会被按越权报警——报反的告警比不报更糟。
	if !errors.Is(err, ErrMembershipCacheNegativeFallback) {
		t.Errorf("兜底成功必须带 ErrMembershipCacheNegativeFallback，got %q", err)
	}
}

// TestInvalidateMembershipCacheReportsTotalFailure DEL 和兜底都失败时，
// 隔离属性真的没守住，错误必须能让调用方区分出这一种。
func TestInvalidateMembershipCacheReportsTotalFailure(t *testing.T) {
	store := &stubCacheStore{
		delErr: errors.New("redis del timeout"),
		setErr: errors.New("redis set timeout"),
	}
	err := invalidateMembershipCacheIn(store, "sp1", "u1")
	if err == nil {
		t.Fatal("两条路都失败时必须报错")
	}
	// 断言落在**区分**两种情形的那个哨兵上，而不是错误文案里的某个子串。
	// 原先断的是 "fallback"——而「兜底成功」那条信息里也有这个词，删掉整个总失败
	// 分支测试照样绿。改断哨兵之后，两条分支互为对方的变异检测：
	// 哨兵漏加、错加、或者两个分支被合并，都会有一条测试立刻红。
	if errors.Is(err, ErrMembershipCacheNegativeFallback) {
		t.Errorf("兜底也失败时不得带 ErrMembershipCacheNegativeFallback——"+
			"这一种是真的隔离失效，必须与兜底成功区分开，got %q", err)
	}
	if !strings.Contains(err.Error(), "also failed") {
		t.Errorf("错误信息也应说明兜底同样失败了，got %q", err)
	}
}
