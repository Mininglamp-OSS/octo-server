package cardactiondispatch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	rd "github.com/go-redis/redis"
)

type QueueConfig struct {
	Prefix       string
	LiveTTL      time.Duration
	DLQRetention time.Duration
}

type Lease struct {
	Event   Event
	Token   string
	Attempt int
}

type NackOutcome string

const (
	NackRequeued      NackOutcome = "requeued"
	NackDeadLettered  NackOutcome = "dead_lettered"
	NackTokenMismatch NackOutcome = "token_mismatch"
)

type QueueDepths struct {
	Ready  int64
	Leased int64
	DLQ    int64
}

type RedisQueue struct {
	client       *rd.Client
	keys         queueKeys
	liveTTL      time.Duration
	dlqRetention time.Duration
}

type queueKeys struct {
	ready      string
	leased     string
	payload    string
	attempts   string
	tokens     string
	dlq        string
	dlqPayload string
	dlqMeta    string
}

var enqueueScript = rd.NewScript(`
if redis.call('HEXISTS', KEYS[7], ARGV[1]) == 1 or redis.call('HEXISTS', KEYS[3], ARGV[1]) == 1 then
  return 0
end
redis.call('HSET', KEYS[3], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[4], ARGV[1], 0)
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[4]) end
return 1
`)

var claimScript = rd.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 1)
if #ids == 0 then return {} end
local id = ids[1]
local payload = redis.call('HGET', KEYS[3], id)
if not payload then
  redis.call('ZREM', KEYS[1], id)
  redis.call('HDEL', KEYS[4], id)
  return {}
end
if redis.call('ZREM', KEYS[1], id) ~= 1 then return {} end
local attempt = redis.call('HINCRBY', KEYS[4], id, 1)
redis.call('ZADD', KEYS[2], ARGV[2], id)
redis.call('HSET', KEYS[5], id, ARGV[3])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[4]) end
return {id, payload, tostring(attempt), ARGV[3]}
`)

var ackScript = rd.NewScript(`
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('HDEL', KEYS[4], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
return 1
`)

var renewScript = rd.NewScript(`
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[2] then return 0 end
if not redis.call('ZSCORE', KEYS[2], ARGV[1]) then return 0 end
redis.call('ZADD', KEYS[2], ARGV[3], ARGV[1])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[4]) end
return 1
`)

var deferScript = rd.NewScript(`
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[2] then return 0 end
if redis.call('ZREM', KEYS[2], ARGV[1]) ~= 1 then return 0 end
redis.call('HDEL', KEYS[5], ARGV[1])
local attempt = tonumber(redis.call('HGET', KEYS[4], ARGV[1]) or '0')
if attempt > 0 then redis.call('HINCRBY', KEYS[4], ARGV[1], -1) end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[4]) end
return 1
`)

var nackScript = rd.NewScript(`
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[2] then return 0 end
local payload = redis.call('HGET', KEYS[3], ARGV[1])
local attempt = tonumber(redis.call('HGET', KEYS[4], ARGV[1]) or '0')
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
if attempt >= tonumber(ARGV[4]) then
	local expired = redis.call('ZRANGEBYSCORE', KEYS[6], '-inf', ARGV[8])
	for _, expired_id in ipairs(expired) do
		redis.call('HDEL', KEYS[7], expired_id)
		redis.call('HDEL', KEYS[8], expired_id)
	end
	redis.call('ZREMRANGEBYSCORE', KEYS[6], '-inf', ARGV[8])
  redis.call('HDEL', KEYS[3], ARGV[1])
  redis.call('HDEL', KEYS[4], ARGV[1])
  redis.call('ZADD', KEYS[6], ARGV[9], ARGV[1])
  redis.call('HSET', KEYS[7], ARGV[1], payload)
  redis.call('HSET', KEYS[8], ARGV[1], ARGV[5])
  for i = 6, 8 do redis.call('PEXPIRE', KEYS[i], ARGV[7]) end
  return 2
end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[6]) end
return 1
`)

var reclaimScript = rd.NewScript(`
local ids = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
for _, id in ipairs(ids) do
  if redis.call('ZREM', KEYS[2], id) == 1 then
    redis.call('HDEL', KEYS[5], id)
    if redis.call('HEXISTS', KEYS[3], id) == 1 then
      redis.call('ZADD', KEYS[1], ARGV[1], id)
    end
  end
end
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[3]) end
return #ids
`)

var replayDLQScript = rd.NewScript(`
local payload = redis.call('HGET', KEYS[7], ARGV[1])
if not payload then return 0 end
local score = redis.call('ZSCORE', KEYS[6], ARGV[1])
if not score or tonumber(score) <= tonumber(ARGV[4]) then
	redis.call('ZREM', KEYS[6], ARGV[1])
	redis.call('HDEL', KEYS[7], ARGV[1])
	redis.call('HDEL', KEYS[8], ARGV[1])
	return 0
end
redis.call('ZREM', KEYS[6], ARGV[1])
redis.call('HDEL', KEYS[7], ARGV[1])
redis.call('HDEL', KEYS[8], ARGV[1])
redis.call('HSET', KEYS[3], ARGV[1], payload)
redis.call('HSET', KEYS[4], ARGV[1], 0)
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[1])
for i = 1, 5 do redis.call('PEXPIRE', KEYS[i], ARGV[3]) end
return 1
`)

var pruneDLQScript = rd.NewScript(`
local expired = redis.call('ZRANGEBYSCORE', KEYS[6], '-inf', ARGV[1])
for _, id in ipairs(expired) do
	redis.call('HDEL', KEYS[7], id)
	redis.call('HDEL', KEYS[8], id)
end
redis.call('ZREMRANGEBYSCORE', KEYS[6], '-inf', ARGV[1])
return #expired
`)

func NewRedisQueue(client *rd.Client, cfg QueueConfig) (*RedisQueue, error) {
	if client == nil {
		return nil, errors.New("cardactiondispatch: Redis client is required")
	}
	if strings.TrimSpace(cfg.Prefix) == "" || strings.ContainsAny(cfg.Prefix, " \t\r\n") {
		return nil, errors.New("cardactiondispatch: invalid queue prefix")
	}
	if cfg.LiveTTL <= 0 || cfg.DLQRetention <= 0 {
		return nil, errors.New("cardactiondispatch: queue retention must be positive")
	}
	base := cfg.Prefix + ":{queue}:"
	return &RedisQueue{
		client: client,
		keys: queueKeys{
			ready:      base + "ready",
			leased:     base + "leased",
			payload:    base + "payload",
			attempts:   base + "attempts",
			tokens:     base + "tokens",
			dlq:        base + "dlq",
			dlqPayload: base + "dlq_payload",
			dlqMeta:    base + "dlq_meta",
		},
		liveTTL:      cfg.LiveTTL,
		dlqRetention: cfg.DLQRetention,
	}, nil
}

func (q *RedisQueue) Enqueue(event Event, due time.Time) error {
	if event.EventID <= 0 || event.SenderUID == "" || event.Owner == "" || event.ActionType == "" {
		return errors.New("cardactiondispatch: invalid queue event")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("cardactiondispatch: marshal event: %w", err)
	}
	_, err = enqueueScript.Run(q.client, q.scriptKeys(),
		strconv.FormatInt(event.EventID, 10), payload, unixMillis(due), durationMillis(q.liveTTL)).Result()
	if err != nil {
		return fmt.Errorf("cardactiondispatch: enqueue event: %w", err)
	}
	return nil
}

func (q *RedisQueue) Claim(now time.Time, leaseDuration time.Duration) (*Lease, error) {
	if leaseDuration <= 0 {
		return nil, errors.New("cardactiondispatch: lease duration must be positive")
	}
	token, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	value, err := claimScript.Run(q.client, q.scriptKeys(),
		unixMillis(now), unixMillis(now.Add(leaseDuration)), token, durationMillis(q.liveTTL)).Result()
	if err != nil {
		return nil, fmt.Errorf("cardactiondispatch: claim event: %w", err)
	}
	items, ok := value.([]interface{})
	if !ok || len(items) == 0 {
		return nil, nil
	}
	if len(items) != 4 {
		return nil, errors.New("cardactiondispatch: malformed Redis claim response")
	}
	payload, ok := redisString(items[1])
	if !ok {
		return nil, errors.New("cardactiondispatch: malformed claimed payload")
	}
	var event Event
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("cardactiondispatch: decode claimed event: %w", err)
	}
	attemptRaw, ok := redisString(items[2])
	if !ok {
		return nil, errors.New("cardactiondispatch: malformed claimed attempt")
	}
	attempt, err := strconv.Atoi(attemptRaw)
	if err != nil {
		return nil, fmt.Errorf("cardactiondispatch: decode claimed attempt: %w", err)
	}
	claimedToken, ok := redisString(items[3])
	if !ok || claimedToken == "" {
		return nil, errors.New("cardactiondispatch: malformed claimed token")
	}
	return &Lease{Event: event, Token: claimedToken, Attempt: attempt}, nil
}

func (q *RedisQueue) Ack(eventID int64, token string) (bool, error) {
	value, err := ackScript.Run(q.client, q.scriptKeys(), strconv.FormatInt(eventID, 10), token).Int()
	if err != nil {
		return false, fmt.Errorf("cardactiondispatch: ack event: %w", err)
	}
	return value == 1, nil
}

func (q *RedisQueue) Renew(eventID int64, token string, now time.Time, leaseDuration time.Duration) (bool, error) {
	if eventID <= 0 || token == "" || leaseDuration <= 0 {
		return false, errors.New("cardactiondispatch: invalid lease renewal")
	}
	value, err := renewScript.Run(q.client, q.scriptKeys(),
		strconv.FormatInt(eventID, 10), token, unixMillis(now.Add(leaseDuration)), durationMillis(q.liveTTL)).Int()
	if err != nil {
		return false, fmt.Errorf("cardactiondispatch: renew lease: %w", err)
	}
	return value == 1, nil
}

// Defer returns a capacity-blocked lease to ready without consuming a delivery
// attempt. The lease token and leased-set membership are checked atomically, so
// a stale worker cannot move a lease owned by another replica.
func (q *RedisQueue) Defer(eventID int64, token string, due time.Time) (bool, error) {
	if eventID <= 0 || token == "" || due.IsZero() {
		return false, errors.New("cardactiondispatch: invalid lease defer")
	}
	value, err := deferScript.Run(q.client, q.scriptKeys(),
		strconv.FormatInt(eventID, 10), token, unixMillis(due), durationMillis(q.liveTTL)).Int()
	if err != nil {
		return false, fmt.Errorf("cardactiondispatch: defer lease: %w", err)
	}
	return value == 1, nil
}

func (q *RedisQueue) Nack(lease Lease, now time.Time, delay time.Duration, maxAttempts int, reason string) (NackOutcome, error) {
	if maxAttempts < 1 || delay < 0 {
		return "", errors.New("cardactiondispatch: invalid nack policy")
	}
	meta, _ := json.Marshal(map[string]interface{}{
		"attempt":   lease.Attempt,
		"reason":    truncate(reason, 256),
		"failed_at": now.Unix(),
	})
	value, err := nackScript.Run(q.client, q.scriptKeys(),
		strconv.FormatInt(lease.Event.EventID, 10), lease.Token, unixMillis(now.Add(delay)), maxAttempts,
		meta, durationMillis(q.liveTTL), durationMillis(q.dlqRetention), unixMillis(now.Add(-q.dlqRetention)), unixMillis(now)).Int()
	if err != nil {
		return "", fmt.Errorf("cardactiondispatch: nack event: %w", err)
	}
	switch value {
	case 0:
		return NackTokenMismatch, nil
	case 1:
		return NackRequeued, nil
	case 2:
		return NackDeadLettered, nil
	default:
		return "", errors.New("cardactiondispatch: malformed Redis nack response")
	}
}

func (q *RedisQueue) ReclaimExpired(now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("cardactiondispatch: reclaim limit must be between 1 and 1000")
	}
	value, err := reclaimScript.Run(q.client, q.scriptKeys(), unixMillis(now), limit, durationMillis(q.liveTTL)).Int()
	if err != nil {
		return 0, fmt.Errorf("cardactiondispatch: reclaim events: %w", err)
	}
	return value, nil
}

func (q *RedisQueue) ReplayDLQ(eventID int64, due time.Time) (bool, error) {
	value, err := replayDLQScript.Run(q.client, q.scriptKeys(),
		strconv.FormatInt(eventID, 10), unixMillis(due), durationMillis(q.liveTTL), unixMillis(due.Add(-q.dlqRetention))).Int()
	if err != nil {
		return false, fmt.Errorf("cardactiondispatch: replay DLQ event: %w", err)
	}
	return value == 1, nil
}

func (q *RedisQueue) Depths() (QueueDepths, error) {
	if _, err := pruneDLQScript.Run(q.client, q.scriptKeys(), unixMillis(time.Now().Add(-q.dlqRetention))).Result(); err != nil {
		return QueueDepths{}, fmt.Errorf("cardactiondispatch: prune expired DLQ events: %w", err)
	}
	pipe := q.client.Pipeline()
	ready := pipe.ZCard(q.keys.ready)
	leased := pipe.ZCard(q.keys.leased)
	dlq := pipe.ZCard(q.keys.dlq)
	if _, err := pipe.Exec(); err != nil {
		return QueueDepths{}, fmt.Errorf("cardactiondispatch: read queue depths: %w", err)
	}
	return QueueDepths{Ready: ready.Val(), Leased: leased.Val(), DLQ: dlq.Val()}, nil
}

func (q *RedisQueue) scriptKeys() []string {
	return []string{
		q.keys.ready, q.keys.leased, q.keys.payload, q.keys.attempts,
		q.keys.tokens, q.keys.dlq, q.keys.dlqPayload, q.keys.dlqMeta,
	}
}

func newLeaseToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("cardactiondispatch: generate lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func redisString(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return "", false
	}
}

func unixMillis(value time.Time) int64 {
	return value.UnixNano() / int64(time.Millisecond)
}

func durationMillis(value time.Duration) int64 {
	return int64(value / time.Millisecond)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}
