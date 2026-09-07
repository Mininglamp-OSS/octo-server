package bot_task

import (
	"golang.org/x/time/rate"
)

// sourceLocalFloor keeps the authenticated per-source quota effective when
// Redis is unavailable. The configured source registry bounds this map, and
// rate.Limiter is safe for concurrent use.
type sourceLocalFloor struct {
	limiters map[string]*rate.Limiter
}

func newSourceLocalFloor(sources sourceRegistry, rps float64, burst int) *sourceLocalFloor {
	limiters := make(map[string]*rate.Limiter, len(sources))
	for source, cfg := range sources {
		if cfg.Enabled {
			limiters[source] = rate.NewLimiter(rate.Limit(rps), burst)
		}
	}
	return &sourceLocalFloor{limiters: limiters}
}

func (f *sourceLocalFloor) Allow(source string) bool {
	if f == nil {
		return true
	}
	limiter, ok := f.limiters[source]
	return ok && limiter.Allow()
}
