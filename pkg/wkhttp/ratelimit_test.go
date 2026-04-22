package wkhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestRedis 连接到 testenv-redis-1，并在 setup/teardown 只清理 ratelimit:* 前缀。
// 之所以不 FlushDB：这份 Redis 容器可能与其他测试共用，全库 flush 会误伤无关数据。
// 项目未引入 miniredis，直接使用 testenv 上的真实 Redis 容器（127.0.0.1:6399）。
func newTestRedis(t *testing.T) *rd.Client {
	t.Helper()
	c := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6399"})
	if err := c.Ping().Err(); err != nil {
		t.Skipf("testenv redis unavailable at 127.0.0.1:6399: %v", err)
	}
	cleanRateLimitKeys(t, c)
	t.Cleanup(func() {
		cleanRateLimitKeys(t, c)
		_ = c.Close()
	})
	return c
}

// cleanRateLimitKeys 扫描并删除所有 ratelimit:* 键。testenv keyspace 很小，
// KEYS 的 O(N) 阻塞可接受；生产代码不应这样写。
func cleanRateLimitKeys(t *testing.T, c *rd.Client) {
	t.Helper()
	keys, err := c.Keys("ratelimit:*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, c.Del(keys...).Err())
	}
}

// newDeadRedis 返回指向不可达地址的 client，用于验证 fail-open 行为。
func newDeadRedis(t *testing.T) *rd.Client {
	t.Helper()
	// 指向一个大概率不会监听的端口，1 次重试后快速失败
	return rd.NewClient(&rd.Options{
		Addr:       "127.0.0.1:1",
		MaxRetries: 0,
	})
}

func TestRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	t.Run("allows requests within limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 10, 10))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "1.2.3.4:1234"
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 2))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		blocked := 0
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "5.6.7.8:1234"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0)
	})

	t.Run("excludes configured paths", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 1, "/health"))
		r.GET("/health", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		for i := 0; i < 20; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/health", nil)
			req.RemoteAddr = "9.9.9.9:1234"
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("isolates rate limits per IP", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 2))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "10.0.0.1:1234"
			r.ServeHTTP(w, req)
		}

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		r.ServeHTTP(w, req)
		assert.Equal(t, 200, w.Code)
	})

	t.Run("X-Real-Ip takes priority over X-Forwarded-For", func(t *testing.T) {
		ip := getClientIP(&http.Request{
			Header: http.Header{
				"X-Real-Ip":       {"3.3.3.3"},
				"X-Forwarded-For": {"spoofed, 1.1.1.1, 2.2.2.2"},
			},
			RemoteAddr: "127.0.0.1:80",
		})
		assert.Equal(t, "3.3.3.3", ip)
	})

	t.Run("falls back to X-Forwarded-For rightmost when no X-Real-Ip", func(t *testing.T) {
		ip := getClientIP(&http.Request{
			Header:     http.Header{"X-Forwarded-For": {"spoofed, 1.1.1.1, 2.2.2.2"}},
			RemoteAddr: "127.0.0.1:80",
		})
		assert.Equal(t, "2.2.2.2", ip)
	})

	t.Run("fail-closed when no IP available", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 1))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		blocked := 0
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = ""
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0)
	})

	t.Run("sets X-RateLimit headers on successful request", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 10, 20))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "1.1.1.1:1234"
		r.ServeHTTP(w, req)

		assert.Equal(t, "20", w.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "ip", w.Header().Get("X-RateLimit-Scope"))
		remaining, err := strconv.Atoi(w.Header().Get("X-RateLimit-Remaining"))
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, remaining, 0)
		assert.Less(t, remaining, 20)
	})

	t.Run("sets Retry-After header on 429", func(t *testing.T) {
		client := newTestRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 1))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		got429 := false
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "2.2.2.2:1234"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
				assert.NoError(t, err)
				assert.GreaterOrEqual(t, retryAfter, 1)
				got429 = true
				break
			}
		}
		assert.True(t, got429, "expected at least one 429 response")
	})

	// fail-open：Redis 不可达时放行，不因基础设施抖动导致 HTTP 503。
	// 监控应基于 redis 客户端自身的 metrics / 日志来报警，而非 HTTP 层的 429。
	t.Run("fails open when redis is unreachable", func(t *testing.T) {
		client := newDeadRedis(t)
		r := gin.New()
		r.Use(RateLimitMiddleware(ctx, client, 1, 1))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "7.7.7.7:1234"
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code, "redis down should fail-open")
		}
	})

	// 跨 middleware 实例共享 Redis 状态：验证"滚动部署 / 多副本"的核心诉求。
	// 这是旧内存实现完全没法做到的行为，也是本次重写的主要动机。
	t.Run("shares state across middleware instances (multi-replica)", func(t *testing.T) {
		client := newTestRedis(t)

		newRouter := func() *gin.Engine {
			r := gin.New()
			r.Use(RateLimitMiddleware(ctx, client, 1, 2))
			r.GET("/test", func(c *gin.Context) {
				c.JSON(200, gin.H{"ok": true})
			})
			return r
		}

		blocked := 0
		for i := 0; i < 10; i++ {
			r := newRouter() // 每次新实例，模拟不同 pod
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = "8.8.8.8:1234"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0, "expected cross-instance blocking via shared redis state")
	})
}

func TestUIDRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	// newTestRouter 将 libwkhttp 中间件桥接到 gin engine。
	// 注意：测试中刻意在多个 router 上复用同一个 mw 实例（见 "isolates rate limits per uid"），
	// 以验证限流状态是挂在底层 Redis、不随 router 变化。
	newTestRouter := func(mw libwkhttp.HandlerFunc, uid string) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if uid != "" {
				c.Set("uid", uid)
			}
			c.Next()
		})
		r.Use(func(c *gin.Context) {
			lc := &libwkhttp.Context{Context: c}
			mw(lc)
		})
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})
		return r
	}

	t.Run("allows requests within limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(UIDRateLimitMiddleware(ctx, client, 10, 10), "user1")
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(UIDRateLimitMiddleware(ctx, client, 1, 2), "user2")
		blocked := 0
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0)
	})

	t.Run("isolates rate limits per uid", func(t *testing.T) {
		client := newTestRedis(t)
		mw := UIDRateLimitMiddleware(ctx, client, 1, 2)

		r1 := newTestRouter(mw, "user3")
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r1.ServeHTTP(w, req)
		}

		r2 := newTestRouter(mw, "user4")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r2.ServeHTTP(w, req)
		assert.Equal(t, 200, w.Code)
	})

	t.Run("skips when uid is absent", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(UIDRateLimitMiddleware(ctx, client, 1, 1), "")
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("sets X-RateLimit headers on successful request", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(UIDRateLimitMiddleware(ctx, client, 10, 20), "user5")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, "20", w.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"))
	})

	t.Run("fails open when redis is unreachable", func(t *testing.T) {
		client := newDeadRedis(t)
		r := newTestRouter(UIDRateLimitMiddleware(ctx, client, 1, 1), "user6")
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})
}

func TestStrictIPRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	newTestRouter := func(mw libwkhttp.HandlerFunc) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			lc := &libwkhttp.Context{Context: c}
			mw(lc)
		})
		r.POST("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})
		return r
	}

	t.Run("allows requests within limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(StrictIPRateLimitMiddleware(ctx, client, "login", 10, 10))
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "1.1.1.1:1000"
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(StrictIPRateLimitMiddleware(ctx, client, "login", 1, 2))
		blocked := 0
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "2.2.2.2:1000"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0)
	})

	t.Run("isolates rate limits per IP", func(t *testing.T) {
		client := newTestRedis(t)
		mw := StrictIPRateLimitMiddleware(ctx, client, "login", 1, 2)
		r := newTestRouter(mw)

		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "3.3.3.3:1000"
			r.ServeHTTP(w, req)
		}

		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)
		req.RemoteAddr = "4.4.4.4:1000"
		r.ServeHTTP(w, req)
		assert.Equal(t, 200, w.Code)
	})

	// 不同 tag 互相隔离：登录组耗尽配额后，注册组不应受影响。
	// 这是引入 tag 参数的核心动机（原内存实现靠独立 sync.Map，Redis 需要 keyspace 分离）。
	t.Run("isolates rate limits per tag", func(t *testing.T) {
		client := newTestRedis(t)
		login := StrictIPRateLimitMiddleware(ctx, client, "login", 1, 2)
		register := StrictIPRateLimitMiddleware(ctx, client, "register", 1, 2)

		rl := newTestRouter(login)
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "6.6.6.6:1000"
			rl.ServeHTTP(w, req)
		}

		rr := newTestRouter(register)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/test", nil)
		req.RemoteAddr = "6.6.6.6:1000"
		rr.ServeHTTP(w, req)
		assert.Equal(t, 200, w.Code, "different tag must not share quota")
	})

	t.Run("fail-closed when no IP available", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(StrictIPRateLimitMiddleware(ctx, client, "login", 1, 1))
		blocked := 0
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = ""
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				blocked++
			}
		}
		assert.Greater(t, blocked, 0)
	})

	t.Run("sets X-RateLimit and Retry-After headers", func(t *testing.T) {
		client := newTestRedis(t)
		r := newTestRouter(StrictIPRateLimitMiddleware(ctx, client, "login", 1, 1))

		w1 := httptest.NewRecorder()
		req1 := httptest.NewRequest("POST", "/test", nil)
		req1.RemoteAddr = "5.5.5.5:1000"
		r.ServeHTTP(w1, req1)
		assert.Equal(t, 200, w1.Code)
		assert.Equal(t, "1", w1.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, "strict:login", w1.Header().Get("X-RateLimit-Scope"))

		got429 := false
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "5.5.5.5:1000"
			r.ServeHTTP(w, req)
			if w.Code == http.StatusTooManyRequests {
				retryAfter, err := strconv.Atoi(w.Header().Get("Retry-After"))
				assert.NoError(t, err)
				assert.GreaterOrEqual(t, retryAfter, 1)
				got429 = true
				break
			}
		}
		assert.True(t, got429, "expected at least one 429")
	})

	t.Run("fails open when redis is unreachable", func(t *testing.T) {
		client := newDeadRedis(t)
		r := newTestRouter(StrictIPRateLimitMiddleware(ctx, client, "login", 1, 1))
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/test", nil)
			req.RemoteAddr = "7.7.7.7:1000"
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})
}
