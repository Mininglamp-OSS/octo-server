package wkhttp

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("allows requests within limit", func(t *testing.T) {
		r := gin.New()
		r.Use(RateLimitMiddleware(10, 10))
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
		r := gin.New()
		r.Use(RateLimitMiddleware(1, 2))
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
		r := gin.New()
		r.Use(RateLimitMiddleware(1, 1, "/health"))
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
		r := gin.New()
		r.Use(RateLimitMiddleware(1, 2))
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
		r := gin.New()
		r.Use(RateLimitMiddleware(1, 1))
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
		r := gin.New()
		r.Use(RateLimitMiddleware(10, 20))
		r.GET("/test", func(c *gin.Context) {
			c.JSON(200, gin.H{"ok": true})
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "1.1.1.1:1234"
		r.ServeHTTP(w, req)

		assert.Equal(t, "20", w.Header().Get("X-RateLimit-Limit"))
		remaining, err := strconv.Atoi(w.Header().Get("X-RateLimit-Remaining"))
		assert.NoError(t, err)
		assert.GreaterOrEqual(t, remaining, 0)
		assert.Less(t, remaining, 20)
	})

	t.Run("sets Retry-After header on 429", func(t *testing.T) {
		r := gin.New()
		r.Use(RateLimitMiddleware(1, 1))
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
}

func TestUIDRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// newTestRouter wraps the libwkhttp middleware into a gin handler, simulating
	// the same bridging done by libwkhttp.WKHttp.
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
		r := newTestRouter(UIDRateLimitMiddleware(10, 10), "user1")
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("blocks requests exceeding limit", func(t *testing.T) {
		r := newTestRouter(UIDRateLimitMiddleware(1, 2), "user2")
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
		mw := UIDRateLimitMiddleware(1, 2)

		// Exhaust user3's quota
		r1 := newTestRouter(mw, "user3")
		for i := 0; i < 5; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r1.ServeHTTP(w, req)
		}

		// user4 should still have quota
		r2 := newTestRouter(mw, "user4")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r2.ServeHTTP(w, req)
		assert.Equal(t, 200, w.Code)
	})

	t.Run("skips when uid is absent", func(t *testing.T) {
		r := newTestRouter(UIDRateLimitMiddleware(1, 1), "")
		// Without uid, middleware should not limit (misconfiguration fail-open)
		for i := 0; i < 10; i++ {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			r.ServeHTTP(w, req)
			assert.Equal(t, 200, w.Code)
		}
	})

	t.Run("sets X-RateLimit headers on successful request", func(t *testing.T) {
		r := newTestRouter(UIDRateLimitMiddleware(10, 20), "user5")
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)

		assert.Equal(t, "20", w.Header().Get("X-RateLimit-Limit"))
	})
}
