package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	rd "github.com/go-redis/redis"
)

var (
	ensureGenerationScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
  redis.call("SET", KEYS[1], ARGV[1], "PX", ARGV[2], "NX")
  current = redis.call("GET", KEYS[1])
end
if not current then
  return redis.error_reply("generation initialization lost")
end
local ttl = redis.call("PTTL", KEYS[1])
if ttl >= 0 and ttl < tonumber(ARGV[2]) then
  redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return current
`)
	addSessionIndexScript = rd.NewScript(`
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[1])
local existing = redis.call("ZSCORE", KEYS[1], ARGV[2])
if not existing and redis.call("ZCARD", KEYS[1]) >= tonumber(ARGV[4]) then
  return -1
end
redis.call("ZADD", KEYS[1], ARGV[3], ARGV[2])
local ttl = redis.call("PTTL", KEYS[1])
if ttl >= 0 and ttl < tonumber(ARGV[5]) then
  redis.call("PEXPIRE", KEYS[1], ARGV[5])
elseif ttl == -1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[5])
end
return 1
`)
	rotateGenerationScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
local revision = 0
local last_version = 0
local ttl = -2
local previous_generation = ""
if current then
  local decoded = cjson.decode(current)
  revision = tonumber(decoded.revision) or 0
  last_version = tonumber(decoded.last_event_version) or 0
  if last_version >= tonumber(ARGV[2]) then
    if last_version == tonumber(ARGV[2]) and decoded.last_event_id == ARGV[3] then
      return {0, decoded.last_revoked_generation or ""}
    end
    return {-1, ""}
  end
  previous_generation = decoded.generation or ""
  ttl = redis.call("PTTL", KEYS[1])
end
local next = cjson.encode({
  generation = ARGV[1],
  revision = revision + 1,
  state = "active",
  last_event_version = tonumber(ARGV[2]),
  last_event_id = ARGV[3],
  last_revoked_generation = previous_generation
})
if ttl == -1 then
  redis.call("SET", KEYS[1], next)
else
  local next_ttl = tonumber(ARGV[4])
  if ttl > next_ttl then
    next_ttl = ttl
  end
  redis.call("SET", KEYS[1], next, "PX", next_ttl)
end
return {1, previous_generation}
`)
	extendGenerationDeadlineScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
  return 0
end
local decoded = cjson.decode(current)
if decoded.generation ~= ARGV[1] or tonumber(decoded.revision) ~= tonumber(ARGV[2]) or decoded.state ~= "active" then
  return 0
end
local ttl = redis.call("PTTL", KEYS[1])
if ttl >= 0 and ttl < tonumber(ARGV[3]) then
  redis.call("PEXPIRE", KEYS[1], ARGV[3])
end
return 1
`)
	casReplacePayloadKeepDeadlineScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
  return {-2, 0, 0}
end
if current ~= ARGV[1] then
  return {-4, 0, 0}
end
local ttl = redis.call("PTTL", KEYS[1])
local persistent = 0
if ttl == -1 then
  ttl = tonumber(ARGV[3])
  persistent = 1
elseif ttl <= 0 then
  return {-2, 0, 0}
elseif ttl > tonumber(ARGV[3]) then
  ttl = tonumber(ARGV[3])
end
redis.call("SET", KEYS[1], ARGV[2], "PX", ttl, "XX")
return {1, ttl, persistent}
`)
)

var (
	ErrV3SessionsDisabled        = errors.New("auth: v3 session writer is disabled")
	ErrSessionLimitReached       = errors.New("auth: session limit reached")
	ErrIssueFenceChanged         = errors.New("auth: session issue fence changed")
	ErrRevocationAlreadyApplied  = errors.New("auth: revocation event already applied")
	ErrSessionGenerationInactive = errors.New("auth: session generation is not active")
	ErrLegacySessionDenied       = errors.New("auth: legacy session denied")
)

const sessionGenerationActive = "active"

type IssueFence struct {
	Generation string
	Revision   uint64
}

type RevocationEvent struct {
	Version uint64
	ID      string
}

type sessionGenerationRecord struct {
	Generation            string `json:"generation"`
	Revision              uint64 `json:"revision"`
	State                 string `json:"state"`
	LastEventVersion      uint64 `json:"last_event_version,omitempty"`
	LastEventID           string `json:"last_event_id,omitempty"`
	LastRevokedGeneration string `json:"last_revoked_generation,omitempty"`
}

type sessionIndexMember struct {
	Token      string `json:"token"`
	DeviceFlag int    `json:"device_flag"`
	DeviceID   string `json:"device_id,omitempty"`
}

// Mode is the phase this replica is currently running at. It can change
// underneath a caller when the floor advances, so read it once per decision
// rather than caching it.
func (s *RedisSessionStore) Mode() SessionMode {
	return s.currentMode()
}

func (s *RedisSessionStore) BeginIssue(ctx context.Context, uid string) (fence IssueFence, err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("begin_issue", started, err) }()
	if !s.currentMode().writesV3() {
		return IssueFence{}, ErrV3SessionsDisabled
	}
	if strings.TrimSpace(uid) == "" {
		return IssueFence{}, errors.New("auth: begin issue requires uid")
	}
	if err := ctx.Err(); err != nil {
		return IssueFence{}, err
	}

	generation, err := newSessionGeneration()
	if err != nil {
		return IssueFence{}, err
	}
	candidate, err := json.Marshal(sessionGenerationRecord{
		Generation: generation,
		Revision:   1,
		State:      sessionGenerationActive,
	})
	if err != nil {
		return IssueFence{}, fmt.Errorf("auth: encode initial session generation: %w", err)
	}
	result, err := ensureGenerationScript.Run(
		s.client,
		[]string{s.generationKey(uid)},
		string(candidate),
		s.maxTTL.Milliseconds(),
	).Result()
	if err != nil {
		return IssueFence{}, fmt.Errorf("auth: initialize session generation: %w", err)
	}
	record, err := decodeSessionGeneration(result)
	if err != nil {
		return IssueFence{}, err
	}
	if record.State != sessionGenerationActive {
		return IssueFence{}, ErrSessionGenerationInactive
	}
	return IssueFence{Generation: record.Generation, Revision: record.Revision}, nil
}

func (s *RedisSessionStore) CurrentGeneration(ctx context.Context, uid string) (string, error) {
	if strings.TrimSpace(uid) == "" {
		return "", errors.New("auth: current generation requires uid")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	raw, err := s.client.Get(s.generationKey(uid)).Result()
	if err == rd.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("auth: load session generation: %w", err)
	}
	record, err := decodeSessionGeneration(raw)
	if err != nil {
		return "", err
	}
	if record.State != sessionGenerationActive {
		return "", ErrSessionGenerationInactive
	}
	return record.Generation, nil
}

func (s *RedisSessionStore) ValidateLegacySession(ctx context.Context, info TokenInfo, record TokenRecord) error {
	// One atomic read for the whole decision: the floor poller may raise the
	// mode mid-request, and a torn read could apply enforce's rejection with
	// bounded's TTL rule (or neither).
	mode := s.currentMode()
	if mode == SessionModeEnforce {
		return ErrLegacySessionDenied
	}
	if mode == SessionModeBounded && (record.TTL <= 0 || record.TTL > s.maxTTL) {
		return ErrLegacySessionDenied
	}
	if !mode.checksLegacyDeny() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := s.client.Exists(s.legacyDenyKey(info.UID)).Result()
	if err != nil {
		return fmt.Errorf("auth: load legacy session deny marker: %w", err)
	}
	if exists != 0 {
		return ErrLegacySessionDenied
	}
	return nil
}

// ReuseSession refreshes an existing credential without extending its
// deadline. In v3 writer modes it also promotes a finite legacy credential to
// v3 under the supplied issuance fence; a missing credential returns false so
// the caller can issue a new random token.
func (s *RedisSessionStore) ReuseSession(ctx context.Context, token string, snapshot TokenInfo, fence IssueFence) (ok bool, err error) {
	if _, stateErr := s.writableState(); stateErr != nil {
		return false, stateErr
	}
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("reuse_session", started, err) }()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(snapshot.UID) == "" || snapshot.DeviceFlag < 0 {
		return false, errors.New("auth: reuse session requires token, uid, and non-negative device flag")
	}
	if !s.currentMode().writesV3() {
		payload, err := Encode(snapshot)
		if err != nil {
			return false, err
		}
		return s.ReuseExisting(ctx, token, payload, snapshot.UID, snapshot.DeviceFlag)
	}
	if strings.TrimSpace(fence.Generation) == "" || fence.Revision == 0 {
		return false, errors.New("auth: reuse v3 session requires a valid issue fence")
	}
	if err := s.requireFence(ctx, snapshot.UID, fence); err != nil {
		return false, err
	}

	for attempt := 0; attempt < 4; attempt++ {
		record, err := s.ReadToken(ctx, s.tokenKey(token))
		if err != nil {
			return false, err
		}
		if record.TTL == -2 || strings.TrimSpace(record.Payload) == "" {
			return false, nil
		}
		current, err := Decode(record.Payload)
		if err != nil {
			return false, fmt.Errorf("auth: decode reused token payload: %w", err)
		}
		if current.UID != snapshot.UID {
			return false, errors.New("auth: reused token uid mismatch")
		}
		if current.IsV3() {
			ok, conflict, err := s.reuseExistingV3(ctx, token, record.Payload, current, snapshot, fence)
			if err != nil || ok || !conflict {
				return ok, err
			}
			continue
		}
		ok, conflict, err := s.promoteLegacySession(ctx, token, record, snapshot, fence)
		if err != nil || ok || !conflict {
			return ok, err
		}
	}
	_ = s.cleanupIndexReservation(token, snapshot.UID, snapshot.DeviceFlag, snapshot.DeviceID, fence.Generation)
	return false, errors.New("auth: reused token changed too frequently")
}

// UpdateSessionSnapshot changes only display and authorization snapshot
// fields. v3 security claims are copied from the current payload and the CAS
// prevents a concurrent writer from being overwritten with stale claims.
func (s *RedisSessionStore) UpdateSessionSnapshot(ctx context.Context, token string, snapshot TokenInfo) (ok bool, err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("update_snapshot", started, err) }()
	if strings.TrimSpace(token) == "" {
		return false, errors.New("auth: update session snapshot requires token")
	}
	for attempt := 0; attempt < 4; attempt++ {
		record, err := s.ReadToken(ctx, s.tokenKey(token))
		if err != nil {
			return false, err
		}
		if record.TTL == -2 || strings.TrimSpace(record.Payload) == "" {
			return false, nil
		}
		current, err := Decode(record.Payload)
		if err != nil {
			return false, fmt.Errorf("auth: decode token snapshot: %w", err)
		}
		if snapshot.UID != "" && snapshot.UID != current.UID {
			return false, errors.New("auth: token snapshot uid mismatch")
		}
		current.Name = snapshot.Name
		current.Role = snapshot.Role
		current.Language = snapshot.Language

		var next string
		capTTL := s.maxTTL
		if current.IsV3() {
			next, err = EncodeV3(current)
			capTTL = time.Unix(current.ExpiresAt, 0).Sub(s.now().UTC())
		} else {
			next, err = Encode(current)
		}
		if err != nil {
			return false, err
		}
		if capTTL <= 0 {
			return false, nil
		}
		_, replaced, conflict, err := s.casReplacePayloadKeepDeadline(ctx, token, record.Payload, next, capTTL)
		if err != nil {
			return false, err
		}
		if conflict {
			continue
		}
		return replaced, nil
	}
	return false, errors.New("auth: token snapshot changed too frequently")
}

func (s *RedisSessionStore) reuseExistingV3(ctx context.Context, token, currentPayload string, current, snapshot TokenInfo, fence IssueFence) (ok bool, conflict bool, err error) {
	if current.SessionGeneration != fence.Generation || current.SessionRevision != fence.Revision {
		return false, false, nil
	}
	if current.DeviceFlag != snapshot.DeviceFlag {
		return false, false, errors.New("auth: reused v3 token device flag mismatch")
	}
	current.Name = snapshot.Name
	current.Role = snapshot.Role
	current.Language = snapshot.Language
	next, err := EncodeV3(current)
	if err != nil {
		return false, false, err
	}
	deadline := time.Unix(current.ExpiresAt, 0)
	capTTL := deadline.Sub(s.now().UTC())
	if capTTL <= 0 {
		return false, false, nil
	}
	ttl, replaced, conflict, err := s.casReplacePayloadKeepDeadline(ctx, token, currentPayload, next, capTTL)
	if err != nil {
		return false, false, err
	}
	if conflict {
		return false, true, nil
	}
	if !replaced {
		return false, false, nil
	}
	member, err := encodeSessionIndexMember(token, current.DeviceFlag, current.DeviceID)
	if err != nil {
		return false, false, err
	}
	if err := s.addSessionIndex(current.UID, current.SessionGeneration, member, current.ExpiresAt, ttl); err != nil {
		return false, false, err
	}
	if err := s.client.Set(s.uidTokenKey(current.UID, current.DeviceFlag), token, ttl).Err(); err != nil {
		return false, false, fmt.Errorf("auth: refresh v3 token device index: %w", err)
	}
	if err := s.extendFenceDeadline(ctx, current.UID, fence, deadline); err != nil {
		return false, false, err
	}
	if err := s.requireFence(ctx, current.UID, fence); err != nil {
		return false, false, err
	}
	return true, false, nil
}

func (s *RedisSessionStore) promoteLegacySession(ctx context.Context, token string, record TokenRecord, snapshot TokenInfo, fence IssueFence) (ok bool, conflict bool, err error) {
	capTTL := record.TTL
	if capTTL == -1 || capTTL > s.maxTTL {
		capTTL = s.maxTTL
	}
	if capTTL <= 0 {
		return false, false, nil
	}
	now := s.now().UTC()
	expiresAt := now.Add(capTTL).Unix()
	deadline := time.Unix(expiresAt, 0)
	capTTL = deadline.Sub(now)
	if capTTL <= 0 {
		return false, false, nil
	}
	snapshot.IssuedAt = now.Unix()
	snapshot.ExpiresAt = expiresAt
	snapshot.SessionGeneration = fence.Generation
	snapshot.SessionRevision = fence.Revision
	payload, err := EncodeV3(snapshot)
	if err != nil {
		return false, false, err
	}
	ownedPayload, err := markIssuedPayload(payload, token)
	if err != nil {
		return false, false, err
	}
	member, err := encodeSessionIndexMember(token, snapshot.DeviceFlag, snapshot.DeviceID)
	if err != nil {
		return false, false, err
	}
	if err := s.addSessionIndex(snapshot.UID, fence.Generation, member, expiresAt, capTTL); err != nil {
		return false, false, err
	}
	ttl, replaced, conflict, err := s.casReplacePayloadKeepDeadline(ctx, token, record.Payload, ownedPayload, capTTL)
	if err != nil {
		if cleanupErr := s.cleanupIndexReservation(token, snapshot.UID, snapshot.DeviceFlag, snapshot.DeviceID, fence.Generation); cleanupErr != nil {
			return false, false, fmt.Errorf("%w (promotion index cleanup failed: %v)", err, cleanupErr)
		}
		return false, false, err
	}
	if conflict {
		if err := s.cleanupIndexReservation(token, snapshot.UID, snapshot.DeviceFlag, snapshot.DeviceID, fence.Generation); err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	if !replaced {
		if err := s.cleanupIndexReservation(token, snapshot.UID, snapshot.DeviceFlag, snapshot.DeviceID, fence.Generation); err != nil {
			return false, false, err
		}
		return false, false, nil
	}
	cleanup := func(primary error) error {
		cleanupErr := s.cleanupIssuedV3(token, ownedPayload, snapshot.UID, snapshot.DeviceFlag, fence.Generation, member)
		if cleanupErr != nil {
			return fmt.Errorf("%w (promoted credential cleanup failed: %v)", primary, cleanupErr)
		}
		return primary
	}
	if err := s.client.Set(s.uidTokenKey(snapshot.UID, snapshot.DeviceFlag), token, ttl).Err(); err != nil {
		return false, false, cleanup(fmt.Errorf("auth: bind promoted token device: %w", err))
	}
	if err := s.extendFenceDeadline(ctx, snapshot.UID, fence, deadline); err != nil {
		return false, false, cleanup(err)
	}
	if err := s.requireFence(ctx, snapshot.UID, fence); err != nil {
		return false, false, cleanup(err)
	}
	return true, false, nil
}

func (s *RedisSessionStore) casReplacePayloadKeepDeadline(ctx context.Context, token, expected, next string, capTTL time.Duration) (ttl time.Duration, replaced bool, conflict bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, false, err
	}
	if capTTL <= 0 {
		return 0, false, false, errors.New("auth: replacement ttl must be positive")
	}
	result, err := casReplacePayloadKeepDeadlineScript.Run(
		s.client,
		[]string{s.tokenKey(token)},
		expected,
		next,
		capTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, false, false, fmt.Errorf("auth: replace token payload: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 3 {
		return 0, false, false, fmt.Errorf("auth: invalid token replacement result %T", result)
	}
	code, err := redisInteger(parts[0])
	if err != nil {
		return 0, false, false, err
	}
	if code == -2 {
		return 0, false, false, nil
	}
	if code == -4 {
		return 0, false, true, nil
	}
	if code != 1 {
		return 0, false, false, fmt.Errorf("auth: unexpected token replacement result %d", code)
	}
	ttlMS, err := redisInteger(parts[1])
	if err != nil {
		return 0, false, false, err
	}
	if ttlMS <= 0 {
		return 0, false, false, fmt.Errorf("auth: invalid token replacement ttl %d", ttlMS)
	}
	persistent, err := redisInteger(parts[2])
	if err != nil {
		return 0, false, false, err
	}
	if persistent == 1 {
		metrics.ObservePersistentSession()
	}
	return time.Duration(ttlMS) * time.Millisecond, true, false, nil
}

// cleanupIndexReservation removes only the caller's speculative bounded-index
// member. A CAS error is ambiguous: the payload write may have succeeded even
// though the client saw an error. Re-reading the token prevents compensation
// from deleting the member currently owned by a successful v3 payload.
func (s *RedisSessionStore) cleanupIndexReservation(token, uid string, deviceFlag int, deviceID, generation string) error {
	member, err := encodeSessionIndexMember(token, deviceFlag, deviceID)
	if err != nil {
		return err
	}
	raw, err := s.client.Get(s.tokenKey(token)).Result()
	if err == nil {
		info, decodeErr := Decode(raw)
		if decodeErr != nil {
			return fmt.Errorf("auth: inspect promotion payload for index cleanup: %w", decodeErr)
		}
		if info.IsV3() {
			currentMember, encodeErr := encodeSessionIndexMember(token, info.DeviceFlag, info.DeviceID)
			if encodeErr != nil {
				return encodeErr
			}
			if currentMember == member {
				return nil
			}
		}
	} else if err != rd.Nil {
		return fmt.Errorf("auth: inspect promotion payload for index cleanup: %w", err)
	}
	if err := s.client.ZRem(s.sessionIndexKey(uid, generation), member).Err(); err != nil {
		return fmt.Errorf("auth: remove promotion index reservation: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) IssueNewSession(ctx context.Context, token string, info TokenInfo, fence IssueFence) (err error) {
	// A writer without a live lease must not create a credential. That is what
	// makes "absent from the writer registry" prove "not writing", which the
	// advance gate relies on. Reads and revocation are untouched: fencing new
	// logins during a Redis outage is the intended degradation.
	if _, stateErr := s.writableState(); stateErr != nil {
		return stateErr
	}
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("issue_v3", started, err) }()
	if !s.currentMode().writesV3() {
		return ErrV3SessionsDisabled
	}
	if strings.TrimSpace(token) == "" || strings.TrimSpace(info.UID) == "" || info.DeviceFlag < 0 {
		return errors.New("auth: issue v3 token requires token, uid, and non-negative device flag")
	}
	if strings.TrimSpace(fence.Generation) == "" || fence.Revision == 0 {
		return errors.New("auth: issue v3 token requires a valid issue fence")
	}
	if err := s.requireFence(ctx, info.UID, fence); err != nil {
		return err
	}

	now := s.now().UTC()
	issuedAt := now.Unix()
	expiresAt := now.Add(s.maxTTL).Unix()
	deadline := time.Unix(expiresAt, 0)
	ttl := deadline.Sub(now)
	if ttl <= 0 {
		return errors.New("auth: configured session lifetime does not reach the next UTC second")
	}
	info.IssuedAt = issuedAt
	info.ExpiresAt = expiresAt
	info.SessionGeneration = fence.Generation
	info.SessionRevision = fence.Revision
	payload, err := EncodeV3(info)
	if err != nil {
		return err
	}
	ownedPayload, err := markIssuedPayload(payload, token)
	if err != nil {
		return fmt.Errorf("auth: mark issued v3 token payload: %w", err)
	}
	member, err := encodeSessionIndexMember(token, info.DeviceFlag, info.DeviceID)
	if err != nil {
		return err
	}

	created, err := s.client.SetNX(s.tokenKey(token), ownedPayload, ttl).Result()
	if err != nil {
		return fmt.Errorf("auth: issue v3 token: %w", err)
	}
	if !created {
		return ErrTokenCollision
	}
	cleanup := func(primary error) error {
		cleanupErr := s.cleanupIssuedV3(token, ownedPayload, info.UID, info.DeviceFlag, fence.Generation, member)
		if cleanupErr != nil {
			return fmt.Errorf("%w (credential cleanup failed: %v)", primary, cleanupErr)
		}
		return primary
	}

	if err := s.addSessionIndex(info.UID, fence.Generation, member, expiresAt, ttl); err != nil {
		return cleanup(err)
	}
	if err := ctx.Err(); err != nil {
		return cleanup(err)
	}
	if err := s.client.Set(s.uidTokenKey(info.UID, info.DeviceFlag), token, ttl).Err(); err != nil {
		return cleanup(fmt.Errorf("auth: bind v3 token device: %w", err))
	}
	if err := s.extendFenceDeadline(ctx, info.UID, fence, deadline); err != nil {
		return cleanup(err)
	}
	if err := s.requireFence(ctx, info.UID, fence); err != nil {
		return cleanup(err)
	}
	return nil
}

func (s *RedisSessionStore) extendFenceDeadline(ctx context.Context, uid string, fence IssueFence, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ttl := deadline.Sub(s.now().UTC())
	if ttl <= 0 {
		return ErrIssueFenceChanged
	}
	result, err := extendGenerationDeadlineScript.Run(
		s.client,
		[]string{s.generationKey(uid)},
		fence.Generation,
		fence.Revision,
		ttl.Milliseconds(),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: extend session generation deadline: %w", err)
	}
	code, err := redisInteger(result)
	if err != nil {
		return err
	}
	if code != 1 {
		return ErrIssueFenceChanged
	}
	return nil
}

func (s *RedisSessionStore) RevokeAll(ctx context.Context, uid string, event RevocationEvent) (err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("revoke_all", started, err) }()
	if !s.currentMode().writesV3() {
		return ErrV3SessionsDisabled
	}
	if strings.TrimSpace(uid) == "" || event.Version == 0 || strings.TrimSpace(event.ID) == "" {
		return errors.New("auth: revoke all requires uid and a versioned event")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	nextGeneration, err := newSessionGeneration()
	if err != nil {
		return err
	}
	result, err := rotateGenerationScript.Run(
		s.client,
		[]string{s.generationKey(uid)},
		nextGeneration,
		event.Version,
		event.ID,
		s.maxTTL.Milliseconds(),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: rotate session generation: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return fmt.Errorf("auth: invalid session generation rotation result %T", result)
	}
	code, err := redisInteger(parts[0])
	if err != nil {
		return err
	}
	previousGeneration, err := redisString(parts[1])
	if err != nil {
		return err
	}
	if code == -1 {
		return ErrRevocationAlreadyApplied
	}
	if code != 0 && code != 1 {
		return fmt.Errorf("auth: unexpected session generation rotation result %d", code)
	}
	if s.currentMode().rank() >= SessionModeRevoke.rank() {
		if err := s.client.Set(s.legacyDenyKey(uid), strconv.FormatUint(event.Version, 10), 0).Err(); err != nil {
			return fmt.Errorf("auth: persist legacy session deny marker: %w", err)
		}
	}
	if previousGeneration != "" {
		if err := s.client.Del(s.sessionIndexKey(uid, previousGeneration)).Err(); err != nil {
			return fmt.Errorf("auth: session generation revoked but index cleanup failed: %w", err)
		}
	}
	if code == 0 {
		return ErrRevocationAlreadyApplied
	}
	return nil
}

// RevokeCurrent removes one bearer with compare-delete semantics and cleans
// both compatibility and v3 indexes only after that exact payload was
// removed. A concurrent payload adoption is retried instead of being deleted
// based on a stale read.
func (s *RedisSessionStore) RevokeCurrent(ctx context.Context, token, uid string, deviceFlag int) (err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("revoke_current", started, err) }()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(uid) == "" || deviceFlag < 0 {
		return errors.New("auth: revoke current requires token, uid, and non-negative device flag")
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		currentPayload, err := s.client.Get(s.tokenKey(token)).Result()
		if err == rd.Nil {
			return s.compareDeleteDeviceIndex(token, uid, deviceFlag)
		}
		if err != nil {
			return fmt.Errorf("auth: load current token for revoke: %w", err)
		}
		info, err := Decode(currentPayload)
		if err != nil {
			return fmt.Errorf("auth: decode current token for revoke: %w", err)
		}
		if info.UID != uid {
			return errors.New("auth: current token uid mismatch")
		}
		result, err := compareDeleteScript.Run(s.client, []string{s.tokenKey(token)}, currentPayload).Result()
		if err != nil {
			return fmt.Errorf("auth: delete current token: %w", err)
		}
		deleted, err := redisInteger(result)
		if err != nil {
			return err
		}
		if deleted == 0 {
			continue
		}
		if err := s.compareDeleteDeviceIndex(token, uid, deviceFlag); err != nil {
			return err
		}
		if info.IsV3() {
			member, err := encodeSessionIndexMember(token, info.DeviceFlag, info.DeviceID)
			if err != nil {
				return err
			}
			if err := s.client.ZRem(s.sessionIndexKey(uid, info.SessionGeneration), member).Err(); err != nil {
				return fmt.Errorf("auth: delete current v3 session index: %w", err)
			}
		}
		return nil
	}
	return errors.New("auth: current token changed too frequently during revoke")
}

// InvalidateCurrentToken is the module-facing current-logout primitive. It
// uses v3 metadata to clean the bounded index; legacy credentials are deleted
// directly because their payload has no trustworthy device scope.
func (s *RedisSessionStore) InvalidateCurrentToken(ctx context.Context, uid, token string) error {
	if strings.TrimSpace(uid) == "" || strings.TrimSpace(token) == "" {
		return errors.New("auth: invalidate current token requires uid and token")
	}
	record, err := s.ReadToken(ctx, s.tokenKey(token))
	if err != nil {
		return err
	}
	if record.TTL == -2 || strings.TrimSpace(record.Payload) == "" {
		return nil
	}
	info, err := Decode(record.Payload)
	if err != nil {
		return fmt.Errorf("auth: decode current token: %w", err)
	}
	if info.UID != uid {
		return errors.New("auth: current token uid mismatch")
	}
	if info.IsV3() {
		return s.RevokeCurrent(ctx, token, uid, info.DeviceFlag)
	}
	if err := s.DeleteToken(ctx, token); err != nil {
		return err
	}
	var cleanupErr error
	for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
		if err := s.compareDeleteDeviceIndex(token, uid, int(flag)); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

func (s *RedisSessionStore) requireFence(ctx context.Context, uid string, fence IssueFence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := s.client.Get(s.generationKey(uid)).Result()
	if err == rd.Nil {
		return ErrIssueFenceChanged
	}
	if err != nil {
		return fmt.Errorf("auth: verify issue fence: %w", err)
	}
	record, err := decodeSessionGeneration(raw)
	if err != nil {
		return err
	}
	if record.State != sessionGenerationActive || record.Generation != fence.Generation || record.Revision != fence.Revision {
		return ErrIssueFenceChanged
	}
	return nil
}

func (s *RedisSessionStore) addSessionIndex(uid, generation, member string, expiresAt int64, ttl time.Duration) error {
	if strings.TrimSpace(generation) == "" {
		return errors.New("auth: add v3 session index requires generation")
	}
	result, err := addSessionIndexScript.Run(
		s.client,
		[]string{s.sessionIndexKey(uid, generation)},
		s.now().UTC().Unix(),
		member,
		expiresAt,
		s.currentMaxPerUID(),
		ttl.Milliseconds(),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: add v3 session index: %w", err)
	}
	code, err := redisInteger(result)
	if err != nil {
		return err
	}
	if code == -1 {
		return ErrSessionLimitReached
	}
	if code != 1 {
		return fmt.Errorf("auth: unexpected session index result %d", code)
	}
	return nil
}

func (s *RedisSessionStore) cleanupIssuedV3(token, ownedPayload, uid string, deviceFlag int, generation, member string) error {
	result, err := compareDeleteScript.Run(s.client, []string{s.tokenKey(token)}, ownedPayload).Result()
	if err != nil {
		return fmt.Errorf("auth: delete issued v3 token: %w", err)
	}
	deleted, err := redisInteger(result)
	if err != nil {
		return err
	}
	if deleted == 0 {
		return nil
	}
	if err := s.compareDeleteDeviceIndex(token, uid, deviceFlag); err != nil {
		return err
	}
	if err := s.client.ZRem(s.sessionIndexKey(uid, generation), member).Err(); err != nil {
		return fmt.Errorf("auth: delete issued v3 session index: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) generationKey(uid string) string {
	return s.uidTokenPrefix + "auth:generation:" + sessionSubject(uid)
}

func (s *RedisSessionStore) sessionIndexKey(uid, generation string) string {
	return s.uidTokenPrefix + "auth:sessions:" + sessionSubject(uid) + ":" + sessionSubject(generation)
}

func (s *RedisSessionStore) legacyDenyKey(uid string) string {
	return s.uidTokenPrefix + "auth:legacy-deny:" + sessionSubject(uid)
}

func sessionSubject(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:])
}

func newSessionGeneration() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate session generation: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSessionGeneration(value interface{}) (sessionGenerationRecord, error) {
	var raw []byte
	switch typed := value.(type) {
	case string:
		raw = []byte(typed)
	case []byte:
		raw = typed
	default:
		return sessionGenerationRecord{}, fmt.Errorf("auth: invalid session generation result %T", value)
	}
	var record sessionGenerationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return sessionGenerationRecord{}, fmt.Errorf("auth: decode session generation: %w", err)
	}
	if strings.TrimSpace(record.Generation) == "" || record.Revision == 0 || strings.TrimSpace(record.State) == "" {
		return sessionGenerationRecord{}, errors.New("auth: invalid session generation record")
	}
	return record, nil
}

func redisString(value interface{}) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	default:
		return "", fmt.Errorf("auth: invalid redis string result %T", value)
	}
}

func encodeSessionIndexMember(token string, deviceFlag int, deviceID string) (string, error) {
	encoded, err := json.Marshal(sessionIndexMember{Token: token, DeviceFlag: deviceFlag, DeviceID: deviceID})
	if err != nil {
		return "", fmt.Errorf("auth: encode session index member: %w", err)
	}
	return string(encoded), nil
}
