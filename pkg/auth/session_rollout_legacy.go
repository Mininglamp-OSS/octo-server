package auth

// Read-only compatibility for the #725 Redis floor. It is consulted exactly
// once when the MySQL singleton is absent. No runtime path writes, repairs, or
// derives its mode from this key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	rd "github.com/go-redis/redis"
)

var readLegacyRolloutControlScript = rd.NewScript(`
local value = redis.call("GET", KEYS[1])
if not value then
  return {false, -2}
end
return {value, redis.call("PTTL", KEYS[1])}
`)

var (
	ErrRolloutControlChanged = errors.New("auth: session rollout control changed concurrently")
	ErrRolloutFloorNotNext   = errors.New("auth: rollout floor must advance exactly one phase")
)

type SessionRolloutControl struct {
	ModeFloor           SessionMode `json:"mode_floor"`
	WriterVersion       int         `json:"writer_version"`
	ObservationMinGapMS int64       `json:"observation_min_gap_ms"`
	MaxPerUID           int         `json:"max_per_uid,omitempty"`
}

func (s *RedisSessionStore) RolloutControl(ctx context.Context) (*SessionRolloutControl, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := readLegacyRolloutControlScript.Run(s.client, []string{s.rolloutControlKey()}).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: read legacy Redis rollout control: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return nil, fmt.Errorf("auth: invalid legacy rollout control result %T", result)
	}
	ttl, err := redisInteger(parts[1])
	if err != nil {
		return nil, err
	}
	if ttl == -2 {
		return nil, nil
	}
	if ttl != -1 {
		return nil, errors.New("auth: legacy rollout control must not expire")
	}
	var raw string
	switch value := parts[0].(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	default:
		return nil, fmt.Errorf("auth: invalid legacy rollout control payload %T", parts[0])
	}
	return decodeRolloutControl(raw)
}

func decodeRolloutControl(raw string) (*SessionRolloutControl, error) {
	var control SessionRolloutControl
	if err := json.Unmarshal([]byte(raw), &control); err != nil {
		return nil, fmt.Errorf("auth: decode legacy rollout control: %w", err)
	}
	if !control.ModeFloor.valid() || control.ModeFloor == SessionModeExpand ||
		control.WriterVersion != 3 || control.ObservationMinGapMS < 0 ||
		control.MaxPerUID < 0 || control.MaxPerUID > sessionMaxPerUIDLimit ||
		strings.TrimSpace(string(control.ModeFloor)) == "" {
		return nil, errors.New("auth: invalid legacy rollout control record")
	}
	return &control, nil
}

func (s *RedisSessionStore) rolloutControlKey() string {
	return s.uidTokenPrefix + "auth:rollout-control"
}
