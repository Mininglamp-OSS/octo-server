package tokenlifecycle

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const MaxTokenExpire = 720 * time.Hour

// ValidateTokenExpire validates Viper's raw post-env-override value before
// octo-lib GetDuration can silently replace an invalid explicit value with its
// default. Only an actually absent key receives the 720h default.
func ValidateTokenExpire(vp *viper.Viper) (time.Duration, error) {
	const key = "cache.tokenExpire"
	const envKey = "TS_CACHE_TOKENEXPIRE"
	if value, ok := os.LookupEnv(envKey); ok {
		return parseTokenExpire(strings.TrimSpace(value), envKey)
	}
	if !vp.IsSet(key) {
		return MaxTokenExpire, nil
	}
	raw := vp.Get(key)
	value, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("%s must be a duration string with a Go unit, got %T", key, raw)
	}
	return parseTokenExpire(strings.TrimSpace(value), key)
}

func parseTokenExpire(value, source string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", source, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", source)
	}
	// Redis PX and the Session Store Lua contract represent deadlines in whole
	// milliseconds. Accepting a smaller duration would pass startup validation
	// but become PX 0 at write time, making every token write fail at runtime.
	if duration < time.Millisecond {
		return 0, fmt.Errorf("%s must be at least %s", source, time.Millisecond)
	}
	if duration > MaxTokenExpire {
		return 0, fmt.Errorf("%s must not exceed %s", source, MaxTokenExpire)
	}
	return duration, nil
}
