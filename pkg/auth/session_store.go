package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/metrics"
	rd "github.com/go-redis/redis"
)

var (
	readTokenScript = rd.NewScript(`
local value = redis.call("GET", KEYS[1])
if not value then
  return {false, -2}
end
return {value, redis.call("PTTL", KEYS[1])}
`)
	updatePayloadKeepDeadlineScript = rd.NewScript(`
local current = redis.call("GET", KEYS[1])
if not current then
  return {-2, 0}
end
local ttl = redis.call("PTTL", KEYS[1])
if ttl == -2 then
  return {-2, 0}
end
if string.sub(current, 1, 3) == "v3:" and string.sub(ARGV[1], 1, 3) ~= "v3:" then
  return {-3, 0}
end
local persistent = 0
if ttl == -1 then
  ttl = tonumber(ARGV[2])
  persistent = 1
elseif ttl <= 0 then
  return {-2, 0}
elseif ttl > tonumber(ARGV[2]) then
  ttl = tonumber(ARGV[2])
end
redis.call("SET", KEYS[1], ARGV[1], "PX", ttl, "XX")
return {ttl, persistent}
`)
	compareDeleteScript = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
)

const issuedOwnerField = "_octo_issue_owner"

var (
	ErrTokenCollision        = errors.New("auth: generated token already exists")
	ErrTokenVersionDowngrade = errors.New("auth: token payload version downgrade rejected")
)

// SessionObservation contains low-cardinality migration facts only. It never
// includes a token, Redis key, UID, or payload.
type SessionObservation struct {
	ScanID           string `json:"scan_id"`
	ScopeFingerprint string `json:"scope_fingerprint"`
	RedisInstanceID  string `json:"redis_instance_id"`
	Complete         bool   `json:"complete"`
	Total            int64  `json:"total"`
	Missing          int64  `json:"missing"`
	Persistent       int64  `json:"persistent"`
	Finite           int64  `json:"finite"`
	OverMax          int64  `json:"over_max"`
	InvalidTTL       int64  `json:"invalid_ttl"`
	DecodeInvalid    int64  `json:"decode_invalid"`
	ReadErrors       int64  `json:"read_errors"`
	V1               int64  `json:"v1"`
	V2               int64  `json:"v2"`
	V3               int64  `json:"v3"`
}

// RedisSessionStore is the sole v2 credential writer in Release A. It owns
// token and compatibility UIDToken key construction so handlers cannot
// accidentally drop or extend a bearer deadline.
type RedisSessionStore struct {
	client         *rd.Client
	tokenPrefix    string
	uidTokenPrefix string
	maxTTL         time.Duration
	// state is the derived rollout state. It is atomic because the floor poller
	// raises it while requests are in flight: the mode is no longer a fixed
	// startup value read from the environment.
	state atomic.Pointer[derivedRolloutState]
	// lease is the write lease this process holds, once the server wiring binds
	// one. Nil in tests and in the operator subcommands, which do not create
	// credentials.
	lease atomic.Pointer[WriterRegistry]
	// issuanceFenced closes the write path while a mode change is being
	// published. It is deliberately independent of the registry lease: a
	// registry publication failure must stay fenced even if an older lease is
	// still locally fresh.
	issuanceFenced atomic.Bool
	now            func() time.Time
	// redisInstanceIDFn resolves the opaque process identity used to keep a
	// resumable SCAN cursor bound to one Redis process.
	redisInstanceIDFn func() (string, error)
}

type SessionStoreOption func(*RedisSessionStore)

// derivedRolloutState is swapped wholesale so mode and cap can never be read
// as a torn pair: raising the mode into a v3 phase without its cap would be a
// bounded-index write with no bound.
type derivedRolloutState struct {
	mode      SessionMode
	maxPerUID int
}

func WithSessionMode(mode SessionMode) SessionStoreOption {
	return func(store *RedisSessionStore) {
		current := store.currentState()
		store.state.Store(&derivedRolloutState{mode: mode, maxPerUID: current.maxPerUID})
	}
}

func WithSessionMaxPerUID(max int) SessionStoreOption {
	return func(store *RedisSessionStore) {
		current := store.currentState()
		store.state.Store(&derivedRolloutState{mode: current.mode, maxPerUID: max})
	}
}

func (s *RedisSessionStore) currentState() derivedRolloutState {
	if state := s.state.Load(); state != nil {
		return *state
	}
	return derivedRolloutState{mode: SessionModeExpand}
}

func (s *RedisSessionStore) currentMode() SessionMode { return s.currentState().mode }

func (s *RedisSessionStore) currentMaxPerUID() int { return s.currentState().maxPerUID }

// ApplyRolloutState raises the derived state to match a newly observed MySQL
// floor. It only ever moves upward: a stale snapshot or operator rollback must
// not re-admit sessions this replica has already stopped accepting.
//
// A missing or unusable cap does NOT block reader strictness. The cap is
// enforced on the write path (see writableState), so a malformed control row
// cannot loosen validation and credential issuance still fails closed.
func (s *RedisSessionStore) ApplyRolloutState(mode SessionMode, maxPerUID int) error {
	if !mode.valid() {
		return fmt.Errorf("auth: cannot apply invalid session mode %q", mode)
	}
	current := s.currentState()
	if maxPerUID <= 0 {
		maxPerUID = current.maxPerUID
	}
	if maxPerUID > sessionMaxPerUIDLimit {
		maxPerUID = sessionMaxPerUIDLimit
	}
	if mode.rank() < current.mode.rank() {
		// The mode never moves down, but the CAP still has to land. A process
		// running above the floor with no usable cap fences every login, and
		// dropping the cap that arrived alongside a lower mode meant a floor
		// record that finally carried one could never heal it.
		if maxPerUID != current.maxPerUID {
			s.state.Store(&derivedRolloutState{mode: current.mode, maxPerUID: maxPerUID})
		}
		return nil
	}
	s.state.Store(&derivedRolloutState{mode: mode, maxPerUID: maxPerUID})
	return nil
}

// writableState is the single gate every credential-creating path goes
// through. It fails closed on both halves: no live write lease, or a
// v3-writing mode with no usable cap, and a bounded index with no bound is not
// something to guess at.
func (s *RedisSessionStore) writableState() (derivedRolloutState, error) {
	if !s.mayWrite() {
		return derivedRolloutState{}, ErrWriterLeaseLost
	}
	state := s.currentState()
	if state.mode.writesV3() && (state.maxPerUID <= 0 || state.maxPerUID > sessionMaxPerUIDLimit) {
		return derivedRolloutState{}, ErrSessionCapUnavailable
	}
	return state, nil
}

func WithSessionClock(now func() time.Time) SessionStoreOption {
	return func(store *RedisSessionStore) {
		if now != nil {
			store.now = now
		}
	}
}

func NewRedisSessionStore(client *rd.Client, tokenPrefix, uidTokenPrefix string, maxTTL time.Duration, opts ...SessionStoreOption) *RedisSessionStore {
	if client == nil {
		panic("auth: NewRedisSessionStore requires non-nil redis client")
	}
	if maxTTL <= 0 {
		panic("auth: NewRedisSessionStore requires positive max TTL")
	}
	store := &RedisSessionStore{
		client:         client,
		tokenPrefix:    tokenPrefix,
		uidTokenPrefix: uidTokenPrefix,
		maxTTL:         maxTTL,
		now:            time.Now,
	}
	store.state.Store(&derivedRolloutState{mode: SessionModeExpand})
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	initial := store.currentState()
	if !initial.mode.valid() {
		panic("auth: NewRedisSessionStore requires a valid session mode")
	}
	if initial.mode.writesV3() {
		if initial.maxPerUID <= 0 || initial.maxPerUID > sessionMaxPerUIDLimit {
			panic("auth: NewRedisSessionStore requires a bounded max sessions per UID in v3 modes")
		}
		if store.maxTTL < time.Second {
			panic("auth: NewRedisSessionStore requires at least one second max TTL in v3 modes")
		}
	}
	return store
}

func (s *RedisSessionStore) tokenKey(token string) string {
	return s.tokenPrefix + token
}

func (s *RedisSessionStore) uidTokenKey(uid string, deviceFlag int) string {
	return s.uidTokenPrefix + strconv.Itoa(deviceFlag) + uid
}

// IssueNew creates a finite bearer and compatibility device index. The stored
// payload carries an internal ownership marker until another request rewrites
// it through ReuseExisting or UpdatePayloadKeepDeadline. If the index write
// fails, only the still-owned credential is deleted as compensation.
func (s *RedisSessionStore) IssueNew(ctx context.Context, token, payload, uid string, deviceFlag int) (err error) {
	if _, stateErr := s.writableState(); stateErr != nil {
		return stateErr
	}
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("issue", started, err) }()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(payload) == "" || strings.TrimSpace(uid) == "" || deviceFlag < 0 {
		return errors.New("auth: issue token requires token, payload, uid, and non-negative device flag")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ownedPayload, err := markIssuedPayload(payload, token)
	if err != nil {
		return fmt.Errorf("auth: mark issued token payload: %w", err)
	}
	created, err := s.client.SetNX(s.tokenKey(token), ownedPayload, s.maxTTL).Result()
	if err != nil {
		return fmt.Errorf("auth: issue token: %w", err)
	}
	if !created {
		return ErrTokenCollision
	}
	if err := s.client.Set(s.uidTokenKey(uid, deviceFlag), token, s.maxTTL).Err(); err != nil {
		_, cleanupErr := compareDeleteScript.Run(
			s.client,
			[]string{s.tokenKey(token)},
			ownedPayload,
		).Result()
		if cleanupErr != nil {
			return fmt.Errorf("auth: bind token device: %w (credential cleanup failed: %v)", err, cleanupErr)
		}
		return fmt.Errorf("auth: bind token device: %w", err)
	}
	return nil
}

// ReuseExisting updates a bearer without extending its deadline and aligns
// the compatibility index to the same TTL. A missing bearer is never recreated.
func (s *RedisSessionStore) ReuseExisting(ctx context.Context, token, payload, uid string, deviceFlag int) (ok bool, err error) {
	if _, stateErr := s.writableState(); stateErr != nil {
		return false, stateErr
	}
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("reuse", started, err) }()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(payload) == "" || strings.TrimSpace(uid) == "" || deviceFlag < 0 {
		return false, errors.New("auth: reuse token requires token, payload, uid, and non-negative device flag")
	}
	ttl, ok, err := s.updatePayloadKeepDeadline(ctx, token, payload)
	if err != nil || !ok {
		return ok, err
	}
	if err := s.client.Set(s.uidTokenKey(uid, deviceFlag), token, ttl).Err(); err != nil {
		// This was a pre-existing credential, so failure must not delete it.
		return true, fmt.Errorf("auth: refresh token device index: %w", err)
	}
	return true, nil
}

func (s *RedisSessionStore) UpdatePayloadKeepDeadline(ctx context.Context, token, payload string) (ok bool, err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("update_payload", started, err) }()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(payload) == "" {
		return false, errors.New("auth: update token requires token and payload")
	}
	_, ok, err = s.updatePayloadKeepDeadline(ctx, token, payload)
	return ok, err
}

func (s *RedisSessionStore) updatePayloadKeepDeadline(ctx context.Context, token, payload string) (time.Duration, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	result, err := updatePayloadKeepDeadlineScript.Run(
		s.client,
		[]string{s.tokenKey(token)},
		payload,
		s.maxTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, false, fmt.Errorf("auth: update token payload: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return 0, false, fmt.Errorf("auth: invalid token update result %T", result)
	}
	ttlMS, err := redisInteger(parts[0])
	if err != nil {
		return 0, false, err
	}
	persistent, err := redisInteger(parts[1])
	if err != nil {
		return 0, false, err
	}
	if ttlMS == -2 {
		return 0, false, nil
	}
	if ttlMS == -3 {
		return 0, false, ErrTokenVersionDowngrade
	}
	if ttlMS <= 0 {
		return 0, false, fmt.Errorf("auth: update token payload returned invalid ttl %d", ttlMS)
	}
	if persistent == 1 {
		metrics.ObservePersistentSession()
	}
	return time.Duration(ttlMS) * time.Millisecond, true, nil
}

// Probe executes the same read-only, single-key Lua used by the authentication
// hot path. It is intended for startup compatibility checks before accepting
// traffic; it never creates or mutates a Redis key.
func (s *RedisSessionStore) Probe(ctx context.Context) error {
	const probeToken = "__octo_auth_session_lua_probe__"
	if _, err := s.ReadToken(ctx, s.tokenKey(probeToken)); err != nil {
		return fmt.Errorf("auth: probe session read script: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) DeviceToken(ctx context.Context, uid string, deviceFlag int) (string, error) {
	if strings.TrimSpace(uid) == "" || deviceFlag < 0 {
		return "", errors.New("auth: load device token requires uid and non-negative device flag")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token, err := s.client.Get(s.uidTokenKey(uid, deviceFlag)).Result()
	if err == rd.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("auth: load device token: %w", err)
	}
	return token, nil
}

func (s *RedisSessionStore) DeleteToken(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("auth: delete token requires token")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.client.Del(s.tokenKey(token)).Err(); err != nil {
		return fmt.Errorf("auth: delete token: %w", err)
	}
	return nil
}

// RevokeIssued compensates a credential only while it is still owned by its
// original issue attempt. Adoption by another request removes the ownership
// marker atomically with its payload update, making this method a no-op.
func (s *RedisSessionStore) RevokeIssued(ctx context.Context, token, uid string, deviceFlag int) error {
	if strings.TrimSpace(token) == "" || strings.TrimSpace(uid) == "" || deviceFlag < 0 {
		return errors.New("auth: revoke token requires token, uid, and non-negative device flag")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	currentPayload, err := s.client.Get(s.tokenKey(token)).Result()
	if err == rd.Nil {
		return s.compareDeleteDeviceIndex(token, uid, deviceFlag)
	}
	if err != nil {
		return fmt.Errorf("auth: load issued token for revoke: %w", err)
	}
	owner, ok := issuedPayloadOwner(currentPayload)
	if !ok || owner != issueOwner(token) {
		// ReuseExisting and UpdatePayloadKeepDeadline rewrite the canonical
		// payload without the issue marker. Once that happens, another request
		// owns the live credential and the original issuer must not revoke it.
		return nil
	}
	var v3IndexKey string
	var v3IndexMember string
	if strings.HasPrefix(currentPayload, v3Prefix) {
		info, err := Decode(currentPayload)
		if err != nil {
			return fmt.Errorf("auth: decode issued v3 token for revoke: %w", err)
		}
		if info.UID != uid || info.DeviceFlag != deviceFlag {
			return errors.New("auth: issued v3 token revoke metadata mismatch")
		}
		v3IndexMember, err = encodeSessionIndexMember(token, info.DeviceFlag, info.DeviceID)
		if err != nil {
			return err
		}
		v3IndexKey = s.sessionIndexKey(info.UID, info.SessionGeneration)
	}
	result, err := compareDeleteScript.Run(
		s.client,
		[]string{s.tokenKey(token)},
		currentPayload,
	).Result()
	if err != nil {
		return fmt.Errorf("auth: delete issued token: %w", err)
	}
	deleted, err := redisInteger(result)
	if err != nil {
		return fmt.Errorf("auth: decode issued token delete result: %w", err)
	}
	if deleted == 0 {
		// The token changed after the ownership read. Preserve both keys: the
		// concurrent writer may have adopted this credential.
		return nil
	}
	deviceErr := s.compareDeleteDeviceIndex(token, uid, deviceFlag)
	var indexErr error
	if v3IndexKey != "" {
		if err := s.client.ZRem(v3IndexKey, v3IndexMember).Err(); err != nil {
			indexErr = fmt.Errorf("auth: delete issued v3 session index: %w", err)
		}
	}
	return errors.Join(deviceErr, indexErr)
}

func (s *RedisSessionStore) compareDeleteDeviceIndex(token, uid string, deviceFlag int) error {
	if _, err := compareDeleteScript.Run(
		s.client,
		[]string{s.uidTokenKey(uid, deviceFlag)},
		token,
	).Result(); err != nil {
		return fmt.Errorf("auth: delete token device index: %w", err)
	}
	return nil
}

func markIssuedPayload(payload, token string) (string, error) {
	prefix, body, err := versionedPayloadJSON(payload)
	if err != nil {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return "", fmt.Errorf("decode token payload: %w", err)
	}
	if fields == nil {
		return "", errors.New("decode token payload: expected JSON object")
	}
	owner, err := json.Marshal(issueOwner(token))
	if err != nil {
		return "", fmt.Errorf("encode issue owner: %w", err)
	}
	fields[issuedOwnerField] = owner
	marked, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("encode issued token payload: %w", err)
	}
	return prefix + string(marked), nil
}

func issuedPayloadOwner(payload string) (string, bool) {
	_, body, err := versionedPayloadJSON(payload)
	if err != nil {
		return "", false
	}
	var marker struct {
		Owner string `json:"_octo_issue_owner"`
	}
	if err := json.Unmarshal([]byte(body), &marker); err != nil || marker.Owner == "" {
		return "", false
	}
	return marker.Owner, true
}

func versionedPayloadJSON(payload string) (string, string, error) {
	switch {
	case strings.HasPrefix(payload, v2Prefix):
		return v2Prefix, strings.TrimPrefix(payload, v2Prefix), nil
	case strings.HasPrefix(payload, v3Prefix):
		return v3Prefix, strings.TrimPrefix(payload, v3Prefix), nil
	default:
		info, err := Decode(payload)
		if err != nil {
			return "", "", fmt.Errorf("decode token payload: %w", err)
		}
		encoded, err := Encode(info)
		if err != nil {
			return "", "", err
		}
		return v2Prefix, strings.TrimPrefix(encoded, v2Prefix), nil
	}
}

func issueOwner(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}

func (s *RedisSessionStore) ReadToken(ctx context.Context, key string) (record TokenRecord, err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("read", started, err) }()
	if strings.TrimSpace(key) == "" {
		return TokenRecord{}, errors.New("auth: read token requires key")
	}
	if err := ctx.Err(); err != nil {
		return TokenRecord{}, err
	}
	result, err := readTokenScript.Run(s.client, []string{key}).Result()
	if err != nil {
		return TokenRecord{}, fmt.Errorf("auth: read token: %w", err)
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return TokenRecord{}, fmt.Errorf("auth: invalid token read result %T", result)
	}
	ttlMS, err := redisInteger(parts[1])
	if err != nil {
		return TokenRecord{}, err
	}
	if ttlMS == -2 {
		return TokenRecord{TTL: -2}, nil
	}
	payload, ok := parts[0].(string)
	if !ok {
		if bytes, bytesOK := parts[0].([]byte); bytesOK {
			payload = string(bytes)
		} else {
			return TokenRecord{}, fmt.Errorf("auth: invalid token payload result %T", parts[0])
		}
	}
	if ttlMS == -1 {
		return TokenRecord{Payload: payload, TTL: -1}, nil
	}
	return TokenRecord{Payload: payload, TTL: time.Duration(ttlMS) * time.Millisecond}, nil
}

// Observe performs an explicit, read-only cursor scan for migration planning.
// It is never called at process startup. The result is aggregate-only, and
// each batch checks context cancellation so operators can stop it safely.
func (s *RedisSessionStore) Observe(ctx context.Context, batchSize int64) (stats SessionObservation, err error) {
	return s.ObserveRateLimited(ctx, batchSize, 0)
}

// ObserveRateLimited is Observe with an optional minimum interval between
// token reads. Production tooling must pass a positive interval to cap Redis
// load; zero is intended for bounded tests and offline environments.
func (s *RedisSessionStore) ObserveRateLimited(ctx context.Context, batchSize int64, interval time.Duration) (stats SessionObservation, err error) {
	started := time.Now()
	defer func() { metrics.ObserveSessionOperation("observe", started, err) }()
	if batchSize <= 0 || batchSize > 10_000 {
		return SessionObservation{}, fmt.Errorf("auth: observe batch size must be between 1 and 10000")
	}
	if interval < 0 {
		return SessionObservation{}, fmt.Errorf("auth: observe interval must not be negative")
	}
	lease, err := s.acquireRolloutScanLease(ctx, rolloutScanLeaseTTL)
	if err != nil {
		return SessionObservation{}, err
	}
	defer lease.Release()
	scanID, err := newSessionGeneration()
	if err != nil {
		return SessionObservation{}, fmt.Errorf("auth: create observation scan id: %w", err)
	}
	stats.ScanID = scanID
	stats.ScopeFingerprint = s.rolloutObservationScopeFingerprint()
	scanner, err := newInstanceBoundScanner(s)
	if err != nil {
		return stats, fmt.Errorf("auth: identify observation Redis instance: %w", err)
	}
	stats.RedisInstanceID = scanner.InstanceID()
	var throttle <-chan time.Time
	var ticker *time.Ticker
	if interval > 0 {
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		throttle = ticker.C
	}
	var cursor uint64
	pattern := s.tokenPrefix + "*"
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := lease.Err(); err != nil {
			return stats, err
		}
		keys, next, restarted, err := scanner.Scan(ctx, cursor, pattern, batchSize)
		if err != nil {
			return stats, fmt.Errorf("auth: observe scan: %w", err)
		}
		if restarted {
			if err := lease.Confirm(); err != nil {
				return stats, err
			}
			stats, err = s.restartObservation(scanner.InstanceID())
			if err != nil {
				return stats, err
			}
			cursor = 0
			continue
		}
		for _, key := range keys {
			if err := lease.Err(); err != nil {
				return stats, err
			}
			if throttle != nil {
				select {
				case <-ctx.Done():
					return stats, ctx.Err()
				case <-throttle:
				}
			}
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			stats.Total++
			record, err := s.ReadToken(ctx, key)
			if err != nil {
				stats.ReadErrors++
				return stats, fmt.Errorf("auth: observe token record: %w", err)
			}
			if record.TTL == -2 {
				stats.Missing++
				continue
			}
			// Decode BEFORE the shape counters, not after.
			//
			// These counters feed the bounded gate, whose job is to mirror what
			// the bounded reader rejects — and the reader only ever reaches its
			// TTL rules for a session that decoded far enough to be validated at
			// all. Counting undecodable keys as Persistent/OverMax made the gate
			// describe the KEYSPACE instead of the credentials in it, and that
			// difference wedged the whole ladder: one permanent undecodable
			// record held Persistent at 1, bounded refused, bounded is the only
			// route to enforce, migration skips undecodable records by design and
			// nothing expires a key with no TTL. Permanently stuck, with no tool
			// able to clear it.
			if _, err := Decode(record.Payload); err != nil {
				stats.DecodeInvalid++
				continue
			}
			switch {
			case record.TTL == -1:
				stats.Persistent++
			case record.TTL > 0:
				stats.Finite++
				if record.TTL > s.maxTTL {
					stats.OverMax++
				}
			default:
				stats.InvalidTTL++
			}
			switch {
			case strings.HasPrefix(record.Payload, v3Prefix):
				stats.V3++
			case strings.HasPrefix(record.Payload, v2Prefix):
				stats.V2++
			default:
				stats.V1++
			}
		}
		restarted, err = scanner.Check(ctx)
		if err != nil {
			return stats, fmt.Errorf("auth: identify Redis instance after observation batch: %w", err)
		}
		if restarted {
			if err := lease.Confirm(); err != nil {
				return stats, err
			}
			stats, err = s.restartObservation(scanner.InstanceID())
			if err != nil {
				return stats, err
			}
			cursor = 0
			continue
		}
		if err := lease.Confirm(); err != nil {
			return stats, err
		}
		cursor = next
		if cursor == 0 {
			restarted, err = scanner.Check(ctx)
			if err != nil {
				return stats, fmt.Errorf("auth: identify Redis instance before observation completion: %w", err)
			}
			if restarted {
				if err := lease.Confirm(); err != nil {
					return stats, err
				}
				stats, err = s.restartObservation(scanner.InstanceID())
				if err != nil {
					return stats, err
				}
				cursor = 0
				continue
			}
			if err := lease.Confirm(); err != nil {
				return stats, err
			}
			stats.Complete = true
			return stats, nil
		}
	}
}

func (s *RedisSessionStore) restartObservation(redisInstanceID string) (SessionObservation, error) {
	scanID, err := newSessionGeneration()
	if err != nil {
		return SessionObservation{}, fmt.Errorf("auth: restart observation scan id: %w", err)
	}
	return SessionObservation{
		ScanID:           scanID,
		ScopeFingerprint: s.rolloutObservationScopeFingerprint(),
		RedisInstanceID:  redisInstanceID,
	}, nil
}

func redisInteger(value interface{}) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case string:
		result, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("auth: invalid redis integer %q: %w", v, err)
		}
		return result, nil
	case []byte:
		return redisInteger(string(v))
	default:
		return 0, fmt.Errorf("auth: invalid redis integer result %T", value)
	}
}

// UseWriterLease binds the write lease. Once set, creating a new credential
// requires a live lease: that is what makes "absent from the registry" prove
// "not writing", which the advance gate depends on. Validation reads are
// deliberately unaffected — fencing new logins during a Redis outage is the
// intended degradation, logging everyone out is not.
func (s *RedisSessionStore) UseWriterLease(registry *WriterRegistry) {
	s.lease.Store(registry)
}

// mayWrite reports whether this process is allowed to create new credentials.
// With no registry bound (tests, operator subcommands) it is always true.
func (s *RedisSessionStore) mayWrite() bool {
	if s.issuanceFenced.Load() {
		metrics.ObserveSessionWriterFence()
		return false
	}
	registry := s.lease.Load()
	if registry == nil {
		return true
	}
	if registry.MayWrite() {
		return true
	}
	metrics.ObserveSessionWriterFence()
	return false
}

// ApplyAndPublishRolloutState is the sole runtime mode transition. Issuance is
// fenced before the local reader changes; the registry state and lease are
// then published atomically. Any failure keeps the fence closed until a later
// poll successfully republishes the same applied state.
func (s *RedisSessionStore) ApplyAndPublishRolloutState(registry *WriterRegistry, mode SessionMode, maxPerUID int) error {
	current := s.currentState()
	if !s.issuanceFenced.Load() && current.mode == mode && current.maxPerUID == maxPerUID {
		return nil
	}
	s.issuanceFenced.Store(true)
	if err := s.ApplyRolloutState(mode, maxPerUID); err != nil {
		return err
	}
	applied := s.currentMode()
	if registry == nil {
		return errors.New("auth: cannot publish rollout state without writer registry")
	}
	if err := registry.PublishAppliedState(string(applied)); err != nil {
		return fmt.Errorf("auth: publish applied rollout state: %w", err)
	}
	metrics.SetSessionRolloutMode(string(applied))
	s.issuanceFenced.Store(false)
	return nil
}

// CanIssue reports whether this replica may create a credential right now. It
// exists so a caller can check BEFORE a destructive step: paths that revoke an
// old token and then issue a replacement would otherwise turn a lease loss into
// a logout rather than a refused login.
func (s *RedisSessionStore) CanIssue() error {
	_, err := s.writableState()
	return err
}

// Client exposes the shared Redis client for adjacent rollout components (the
// writer registry, the operator subcommands) that must not open a second pool.
func (s *RedisSessionStore) Client() *rd.Client { return s.client }

// UIDTokenPrefix is the namespace all rollout safety state lives under.
func (s *RedisSessionStore) UIDTokenPrefix() string { return s.uidTokenPrefix }

// RedisInstanceFingerprint identifies the Redis process this store is talking
// to. It is printed by every operator subcommand: identity derived purely from
// config cannot tell two endpoints apart, which is how a misplaced config key
// once pointed a tool at the wrong Redis without a word of complaint.
func (s *RedisSessionStore) RedisInstanceFingerprint() (string, error) {
	return s.currentRedisInstanceID()
}

// ForceAdvanceRollout is the operator fault channel, for when the reconciler
// itself is broken. The caller must have already evaluated the predicate, which
// --force does not skip.
//
// The MySQL controller performs the evidence insert and versioned floor CAS in
// one transaction, so this path cannot bypass pause, ordering, or audit rules.
func (s *RedisSessionStore) ForceAdvanceRollout(
	ctx context.Context,
	decision RolloutAdvanceDecision,
	control RolloutStateController,
) error {
	if !decision.Allowed {
		return errors.New("auth: refusing to advance a floor the predicate did not allow")
	}
	if control == nil {
		return errors.New("auth: refusing to advance without MySQL rollout control")
	}
	_, err := control.Advance(ctx, RolloutState{
		Floor: decision.Current, MaxPerUID: decision.MaxPerUID, Version: decision.StateVersion,
	}, rolloutAdvanceRecord(decision, "operator"))
	if err != nil {
		return err
	}
	metrics.ObserveSessionFloorAdvance(string(decision.Target), "operator")
	return nil
}
