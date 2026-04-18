package wkhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
}
