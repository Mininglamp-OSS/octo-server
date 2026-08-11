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

var (
	ErrRolloutControlChanged = errors.New("auth: session rollout control changed concurrently")
	// ErrRolloutFloorNotNext and ErrRolloutFirstFloor are the pre-check's half of
	// a lost race: a racer that read AFTER the winner wrote is rejected here
	// rather than at the CAS. Sentinels rather than error strings, so renaming a
	// message cannot turn a benign race into an alert or mask a real violation.
	ErrRolloutFloorNotNext = errors.New("auth: rollout floor must advance exactly one phase")
	ErrRolloutFirstFloor   = errors.New("auth: first rollout floor must be v3-write")
)

// SessionRolloutControl is the floor record. It stays in Redis under a Lua CAS
// (see session_rollout_marker.go for why it is not moved to MySQL).
//
// ObservationMinGapMS is no longer used — the two-observation ritual it paced
// is gone — but it is still read and carried forward so an existing #725
// record needs no surgery, and still WRITTEN so a #725 artifact can parse the
// record during the rollback window. MaxPerUID is new; #725 unmarshals into a
// struct and ignores unknown fields, so adding it is backward compatible.
// Nothing in this struct may be renamed or removed this release.
type SessionRolloutControl struct {
	ModeFloor           SessionMode `json:"mode_floor"`
	WriterVersion       int         `json:"writer_version"`
	ObservationMinGapMS int64       `json:"observation_min_gap_ms"`
	MaxPerUID           int         `json:"max_per_uid,omitempty"`
}

// RecoverRolloutControlAtEnforce rebuilds a floor record that Redis lost or
// corrupted, at enforce. The caller must have seen the MySQL marker: without
// that proof this is indistinguishable from a fresh install, where the same
// write would log every user out.
//
// A plain SET NX was not enough. A corrupt record still occupies the key, so
// NX left it in place and "recovery" reported success while the floor stayed
// unreadable. Read first: an absent or unreadable record is replaced, a valid
// one that reappeared always wins and is reported as such.
func (s *RedisSessionStore) RecoverRolloutControlAtEnforce(ctx context.Context, maxPerUID int) (recovered bool, err error) {
	if maxPerUID <= 0 || maxPerUID > sessionMaxPerUIDLimit {
		return false, errors.New("auth: rollout recovery requires a bounded max sessions per UID")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	existing, raw, loadErr := s.loadRolloutControl(ctx)
	if loadErr == nil && existing != nil {
		// The floor came back on its own. It wins — recovery must never lower a
		// readable floor, and must not claim to have done anything.
		return false, nil
	}
	encoded, err := json.Marshal(SessionRolloutControl{
		ModeFloor:     SessionModeEnforce,
		WriterVersion: 3,
		MaxPerUID:     maxPerUID,
	})
	if err != nil {
		return false, fmt.Errorf("auth: encode recovered rollout control: %w", err)
	}
	if loadErr != nil {
		// Unreadable: read the byte payload directly so the CAS can replace
		// exactly what was seen and nothing that arrived since.
		current, getErr := s.client.Get(s.rolloutControlKey()).Result()
		if getErr != nil && getErr != rd.Nil {
			return false, fmt.Errorf("auth: read corrupt rollout control: %w", getErr)
		}
		raw = current
	}
	result, err := advanceRolloutControlScript.Run(s.client, []string{s.rolloutControlKey()}, raw, string(encoded)).Result()
	if err != nil {
		return false, fmt.Errorf("auth: recover rollout control: %w", err)
	}
	changed, err := redisInteger(result)
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// EnsureRolloutMaxPerUID writes a cap into a floor record that predates the
// field. #725 records carry no cap, so without this the value only ever lives
// in process memory and is lost on the next restart — after which a v3-writing
// floor has no bound to apply. Idempotent and never lowers an existing cap.
func (s *RedisSessionStore) EnsureRolloutMaxPerUID(ctx context.Context, maxPerUID int) error {
	if maxPerUID <= 0 || maxPerUID > sessionMaxPerUIDLimit {
		return nil
	}
	current, raw, err := s.loadRolloutControl(ctx)
	if err != nil || current == nil || current.MaxPerUID >= maxPerUID {
		return err
	}
	next := *current
	next.MaxPerUID = maxPerUID
	encoded, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("auth: encode rollout control cap: %w", err)
	}
	if _, err := advanceRolloutControlScript.Run(s.client, []string{s.rolloutControlKey()}, raw, string(encoded)).Result(); err != nil {
		return fmt.Errorf("auth: persist rollout control cap: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) RolloutControl(ctx context.Context) (*SessionRolloutControl, error) {
	control, _, err := s.loadRolloutControl(ctx)
	return control, err
}

// AdvanceRolloutControl performs one monotonic CAS transition.
//
// maxPerUID sets the bounded-session cap carried in the record; pass 0 to keep
// the persisted value. It is required when crossing into a v3-writing floor,
// because from that point the cap is no longer supplied by the environment.
//
// A loser in a race surfaces one of two errors and BOTH are benign: a racer
// that read before the winner's write reaches the CAS and gets
// ErrRolloutControlChanged, while one that read after is rejected here by the
// pre-check. Anything racing this (notably a multi-replica reconciler) must
// treat both as a no-op rather than an error worth alerting on.
func (s *RedisSessionStore) AdvanceRolloutControl(ctx context.Context, next SessionMode, maxPerUID int) error {
	if !next.valid() || next == SessionModeExpand {
		return errors.New("auth: rollout floor must advance to v3-write or later")
	}
	current, raw, err := s.loadRolloutControl(ctx)
	if err != nil {
		return err
	}
	observationMinGapMS := int64(0)
	if current == nil {
		if next != SessionModeV3Write {
			return fmt.Errorf("%w (got %s)", ErrRolloutFirstFloor, next)
		}
	} else {
		observationMinGapMS = current.ObservationMinGapMS
		if next.rank() != current.ModeFloor.rank()+1 {
			return fmt.Errorf("%w from %s (got %s)", ErrRolloutFloorNotNext, current.ModeFloor, next)
		}
		if maxPerUID <= 0 {
			maxPerUID = current.MaxPerUID
		}
	}
	if next.writesV3() && (maxPerUID <= 0 || maxPerUID > sessionMaxPerUIDLimit) {
		return fmt.Errorf("auth: rollout floor %s requires a bounded max sessions per UID", next)
	}
	nextControl := SessionRolloutControl{
		ModeFloor:           next,
		WriterVersion:       3,
		ObservationMinGapMS: observationMinGapMS,
		MaxPerUID:           maxPerUID,
	}
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
	if !control.ModeFloor.valid() || control.ModeFloor == SessionModeExpand || control.WriterVersion != 3 || control.ObservationMinGapMS < 0 || strings.TrimSpace(string(control.ModeFloor)) == "" {
		return nil, "", errors.New("auth: invalid rollout control record")
	}
	return &control, raw, nil
}

func (s *RedisSessionStore) rolloutControlKey() string {
	return s.uidTokenPrefix + "auth:rollout-control"
}
