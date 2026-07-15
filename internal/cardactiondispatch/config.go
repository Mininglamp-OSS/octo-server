package cardactiondispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

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

func LoadAllowedURLs(raw string) []string {
	parts := strings.Split(raw, ",")
	urls := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			urls = append(urls, value)
		}
	}
	return urls
}
