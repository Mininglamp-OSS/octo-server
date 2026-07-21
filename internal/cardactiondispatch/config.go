package cardactiondispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultDLQRetention is the fallback dead-letter retention: how long a
	// dead-lettered card-action event stays replayable (via tools/card-action-dlq)
	// before pruning, when the env override is unset or invalid. It matches the
	// value the code shipped with before the retention became configurable, so an
	// upgrade that does not set the override keeps the existing recovery window and
	// never silently prunes older DLQ entries on first deploy. Opt into a shorter
	// window via DLQRetentionEnv.
	DefaultDLQRetention = 30 * 24 * time.Hour
	// DLQRetentionEnv names the retention override, expressed in whole days.
	DLQRetentionEnv = "OCTO_CARD_ACTION_DLQ_RETENTION_DAYS"
	// maxDLQRetentionDays bounds the override so a typo cannot pin the DLQ open for
	// an unreasonable span.
	maxDLQRetentionDays = 365
)

// DLQRetentionFromEnv resolves the dead-letter retention from OCTO_CARD_ACTION_DLQ_RETENTION_DAYS
// (whole days). Empty / non-integer / non-positive / over-max values fall back to
// DefaultDLQRetention so a misconfigured override degrades to a safe window rather than
// truncating the recovery span (NewRedisQueue rejects a non-positive retention outright).
// Shared by main.go and tools/card-action-dlq so the two binaries never drift on the CODE
// value. The server is the pruning authority: it prunes lazily on its own Depths() calls with
// this resolved window. The CLI only ever applies retention on an explicit `replay`; its
// read-only `depth` never prunes (see DepthsNoPrune), so merely inspecting the DLQ from a shell
// that lacks the env var can no longer delete server-retained events.
func DLQRetentionFromEnv(getenv func(string) string) time.Duration {
	raw := strings.TrimSpace(getenv(DLQRetentionEnv))
	if raw == "" {
		return DefaultDLQRetention
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days <= 0 || days > maxDLQRetentionDays {
		return DefaultDLQRetention
	}
	return time.Duration(days) * 24 * time.Hour
}

type routeJSON struct {
	SenderUID      string `json:"sender_uid"`
	Owner          string `json:"owner"`
	ActionType     string `json:"action_type"`
	URL            string `json:"url"`
	SecretEnv      string `json:"secret_env"`
	NotifyTokenEnv string `json:"notify_token_env"`
	TimeoutMS      int64  `json:"timeout_ms"`
	MaxAttempts    int    `json:"max_attempts"`
	BaseBackoffMS  int64  `json:"base_backoff_ms"`
	MaxBackoffMS   int64  `json:"max_backoff_ms"`
	MaxInFlight    int    `json:"max_in_flight"`
}

func LoadRouteSpecs(raw string) ([]RouteSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var encoded []routeJSON
	if err := decoder.Decode(&encoded); err != nil {
		return nil, fmt.Errorf("cardactiondispatch: decode routes: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("cardactiondispatch: trailing route configuration")
	}
	specs := make([]RouteSpec, 0, len(encoded))
	for _, item := range encoded {
		if item.TimeoutMS < 0 || item.BaseBackoffMS < 0 || item.MaxBackoffMS < 0 {
			return nil, errors.New("cardactiondispatch: route durations must not be negative")
		}
		spec := RouteSpec{
			SenderUID:      item.SenderUID,
			Owner:          item.Owner,
			ActionType:     item.ActionType,
			URL:            item.URL,
			SecretEnv:      item.SecretEnv,
			NotifyTokenEnv: item.NotifyTokenEnv,
			MaxAttempts:    item.MaxAttempts,
			MaxInFlight:    item.MaxInFlight,
		}
		if item.TimeoutMS > 0 {
			spec.Timeout = time.Duration(item.TimeoutMS) * time.Millisecond
		}
		if item.BaseBackoffMS > 0 {
			spec.BaseBackoff = time.Duration(item.BaseBackoffMS) * time.Millisecond
		}
		if item.MaxBackoffMS > 0 {
			spec.MaxBackoff = time.Duration(item.MaxBackoffMS) * time.Millisecond
		}
		specs = append(specs, withRouteDefaults(spec))
	}
	return specs, nil
}
