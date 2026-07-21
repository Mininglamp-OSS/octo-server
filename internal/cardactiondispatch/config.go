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
	// before pruning, when the env override is unset or invalid.
	DefaultDLQRetention = 7 * 24 * time.Hour
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
// value — but note both read the process env, so an operator must export the SAME
// OCTO_CARD_ACTION_DLQ_RETENTION_DAYS when running the CLI as the server uses, or the CLI's
// prune-on-read (Depths/ReplayDLQ) will delete events at a different window than the server.
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
