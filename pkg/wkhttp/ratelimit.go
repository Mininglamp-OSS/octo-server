package wkhttp

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen atomic.Int64
}

func getClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if ip, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return ip
	}
	return ""
}

func RateLimitMiddleware(rps float64, burst int, excludePaths ...string) gin.HandlerFunc {
	var limiters sync.Map

	excludeSet := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		excludeSet[p] = struct{}{}
	}

	go func() {
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiters.Range(func(key, value any) bool {
				entry := value.(*ipLimiter)
				if time.Since(time.Unix(0, entry.lastSeen.Load())) > 10*time.Minute {
					limiters.Delete(key)
				}
				return true
			})
		}
	}()

	return func(c *gin.Context) {
		if _, ok := excludeSet[c.Request.URL.Path]; ok {
			c.Next()
			return
		}

		// fail-closed: 拿不到 IP 时走全局桶，不放行
		ip := getClientIP(c.Request)
		if ip == "" {
			ip = "__unknown_ip__"
		}

		val, ok := limiters.Load(ip)
		var entry *ipLimiter
		if ok {
			entry = val.(*ipLimiter)
		} else {
			entry = &ipLimiter{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
			actual, loaded := limiters.LoadOrStore(ip, entry)
			if loaded {
				entry = actual.(*ipLimiter)
			}
		}
		entry.lastSeen.Store(time.Now().UnixNano())

		if !entry.limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"msg":    "请求过于频繁，请稍后再试",
				"status": http.StatusTooManyRequests,
			})
			return
		}

		c.Next()
	}
}
