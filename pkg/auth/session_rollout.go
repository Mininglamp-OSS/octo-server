package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	rd "github.com/go-redis/redis"
)

var (
	readRolloutControlScript = rd.NewScript(`
local value = redis.call("GET", KEYS[1])
if not value then
  return {false, -2}
end
return {value, redis.call("PTTL", KEYS[1])}
`)
	advanceRolloutControlScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
if ARGV[1] == "" then
  if current then
    return 0
  end
  redis.call("SET", KEYS[1], ARGV[2], "NX")
  return 1
end
if current ~= ARGV[1] then
  return 0
end
redis.call("SET", KEYS[1], ARGV[2], "XX")
return 1
`)
)

var ErrRolloutControlChanged = errors.New("auth: session rollout control changed concurrently")

type SessionRolloutControl struct {
	ModeFloor     SessionMode `json:"mode_floor"`
	WriterVersion int         `json:"writer_version"`
}

func (s *RedisSessionStore) ValidateRolloutControl(ctx context.Context, requiredFloor SessionMode) error {
	control, _, err := s.loadRolloutControl(ctx)
	if err != nil {
		return err
	}
	if control == nil {
		if requiredFloor.valid() {
			return fmt.Errorf("auth: session rollout control is required at floor %s", requiredFloor)
		}
		if s.mode.rank() >= SessionModeRevoke.rank() {
			return fmt.Errorf("auth: session rollout control is required for mode %s", s.mode)
		}
		return nil
	}
	if s.mode.rank() < control.ModeFloor.rank() {
		return fmt.Errorf("auth: configured session mode %s is below persisted floor %s", s.mode, control.ModeFloor)
	}
	if requiredFloor.valid() && control.ModeFloor.rank() < requiredFloor.rank() {
		return fmt.Errorf("auth: persisted session floor %s is below required floor %s", control.ModeFloor, requiredFloor)
	}
	return nil
}

func (s *RedisSessionStore) RolloutControl(ctx context.Context) (*SessionRolloutControl, error) {
	control, _, err := s.loadRolloutControl(ctx)
	return control, err
}

// AdvanceRolloutControl performs one monotonic CAS transition. It is intended
// for an explicit operator tool, never an API process startup hook.
func (s *RedisSessionStore) AdvanceRolloutControl(ctx context.Context, next SessionMode) error {
	if !next.valid() || next == SessionModeExpand {
		return errors.New("auth: rollout floor must advance to v3-write or later")
	}
	current, raw, err := s.loadRolloutControl(ctx)
	if err != nil {
		return err
	}
	if current == nil {
		if next != SessionModeV3Write {
			return fmt.Errorf("auth: first rollout floor must be %s", SessionModeV3Write)
		}
	} else {
		if next.rank() != current.ModeFloor.rank()+1 {
			return fmt.Errorf("auth: rollout floor must advance exactly one phase from %s", current.ModeFloor)
		}
	}
	nextControl := SessionRolloutControl{ModeFloor: next, WriterVersion: 3}
	encoded, err := json.Marshal(nextControl)
	if err != nil {
		return fmt.Errorf("auth: encode rollout control: %w", err)
	}
	result, err := advanceRolloutControlScript.Run(s.client, []string{s.rolloutControlKey()}, raw, string(encoded)).Result()
	if err != nil {
		return fmt.Errorf("auth: advance rollout control: %w", err)
	}
	changed, err := redisInteger(result)
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrRolloutControlChanged
	}
	return nil
}

func (s *RedisSessionStore) loadRolloutControl(ctx context.Context) (*SessionRolloutControl, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	result, err := readRolloutControlScript.Run(s.client, []string{s.rolloutControlKey()}).Result()
	if err != nil {
		return nil, "", fmt.Errorf("auth: read rollout control: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return nil, "", fmt.Errorf("auth: invalid rollout control result %T", result)
	}
	ttl, err := redisInteger(parts[1])
	if err != nil {
		return nil, "", err
	}
	if ttl == -2 {
		return nil, "", nil
	}
	if ttl != -1 {
		return nil, "", errors.New("auth: rollout control must not expire")
	}
	var raw string
	switch value := parts[0].(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	default:
		return nil, "", fmt.Errorf("auth: invalid rollout control payload %T", parts[0])
	}
	var control SessionRolloutControl
	if err := json.Unmarshal([]byte(raw), &control); err != nil {
		return nil, "", fmt.Errorf("auth: decode rollout control: %w", err)
	}
	if !control.ModeFloor.valid() || control.ModeFloor == SessionModeExpand || control.WriterVersion != 3 || strings.TrimSpace(string(control.ModeFloor)) == "" {
		return nil, "", errors.New("auth: invalid rollout control record")
	}
	return &control, raw, nil
}

func (s *RedisSessionStore) rolloutControlKey() string {
	return s.uidTokenPrefix + "auth:rollout-control"
}
