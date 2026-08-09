package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	Complete      bool  `json:"complete"`
	Total         int64 `json:"total"`
	Missing       int64 `json:"missing"`
	Persistent    int64 `json:"persistent"`
	Finite        int64 `json:"finite"`
	OverMax       int64 `json:"over_max"`
	InvalidTTL    int64 `json:"invalid_ttl"`
	DecodeInvalid int64 `json:"decode_invalid"`
	ReadErrors    int64 `json:"read_errors"`
	V1            int64 `json:"v1"`
	V2            int64 `json:"v2"`
	V3            int64 `json:"v3"`
}

// RedisSessionStore is the sole v2 credential writer in Release A. It owns
// token and compatibility UIDToken key construction so handlers cannot
// accidentally drop or extend a bearer deadline.
type RedisSessionStore struct {
	client         *rd.Client
	tokenPrefix    string
	uidTokenPrefix string
	maxTTL         time.Duration
	mode           SessionMode
	maxPerUID      int
	now            func() time.Time
}

type SessionStoreOption func(*RedisSessionStore)

func WithSessionMode(mode SessionMode) SessionStoreOption {
	return func(store *RedisSessionStore) {
		store.mode = mode
	}
}

func WithSessionMaxPerUID(max int) SessionStoreOption {
	return func(store *RedisSessionStore) {
		store.maxPerUID = max
	}
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
		mode:           SessionModeExpand,
		now:            time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(store)
		}
	}
	if !store.mode.valid() {
		panic("auth: NewRedisSessionStore requires a valid session mode")
	}
	if store.mode.writesV3() {
		if store.maxPerUID <= 0 || store.maxPerUID > sessionMaxPerUIDLimit {
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
	return s.compareDeleteDeviceIndex(token, uid, deviceFlag)
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
		keys, next, err := s.client.Scan(cursor, pattern, batchSize).Result()
		if err != nil {
			return stats, fmt.Errorf("auth: observe scan: %w", err)
		}
		for _, key := range keys {
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
			switch {
			case record.TTL == -2:
				stats.Missing++
				continue
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

			if _, err := Decode(record.Payload); err != nil {
				stats.DecodeInvalid++
				continue
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
		cursor = next
		if cursor == 0 {
			stats.Complete = true
			return stats, nil
		}
	}
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
