package tokenlifecycle

import (
	"fmt"
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
	if !vp.IsSet(key) {
		return MaxTokenExpire, nil
	}
	raw := vp.Get(key)
	value, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("%s must be a duration string with a Go unit, got %T", key, raw)
	}
	value = strings.TrimSpace(value)
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	if duration > MaxTokenExpire {
		return 0, fmt.Errorf("%s must not exceed %s", key, MaxTokenExpire)
	}
	return duration, nil
}
