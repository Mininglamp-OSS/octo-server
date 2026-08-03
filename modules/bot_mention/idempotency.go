package bot_mention

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	mentionClaimPrefix  = "docbotmention:"
	claimPendingTTL     = 60 * time.Second
	claimRetryAfter     = 2 * time.Second
	defaultClaimDoneTTL = 7 * 24 * time.Hour

	claimRecordPending = "pending"
	claimRecordDone    = "done"
)

var errClaimNotFound = errors.New("bot mention claim not found")

type claimState uint8

const (
	claimMissing claimState = iota
	claimAcquired
	claimPending
	claimReplay
	claimConflict
)

type claimOutcome struct {
	State   claimState
	EventID int64
}

type claimRecord struct {
	State   string `json:"state"`
	SHA     string `json:"sha"`
	Token   string `json:"token,omitempty"`
	EventID int64  `json:"event_id,omitempty"`
}

type claimLease struct {
	key          string
	sha          string
	pendingValue string
}

type claimBackend interface {
	Get(key string) (string, error)
	SetNX(key, value string, ttl time.Duration) (bool, error)
	CompareAndEnqueue(key, oldValue, newValue string, ttl time.Duration, event robot.PreparedBotTypedEvent) (bool, error)
	CompareAndDelete(key, oldValue string) (bool, error)
}

type claimStore struct {
	backend  claimBackend
	doneTTL  time.Duration
	newToken func() (string, error)
}

func newClaimStore(backend claimBackend, doneTTL time.Duration, newToken func() (string, error)) *claimStore {
	if doneTTL <= 0 {
		doneTTL = defaultClaimDoneTTL
	}
	if newToken == nil {
		newToken = newClaimToken
	}
	return &claimStore{backend: backend, doneTTL: doneTTL, newToken: newToken}
}

func newRedisClaimStore(ctx *config.Context) *claimStore {
	doneTTL := ctx.GetConfig().Robot.MessageExpire
	client := octoredis.NewInstrumentedClient(ctx.GetConfig(), func(options *rd.Options) {
		options.MaxRetries = 3
		options.ReadTimeout = 3 * time.Second
		options.WriteTimeout = 3 * time.Second
		options.DialTimeout = 3 * time.Second
	})
	return newClaimStore(&redisClaimBackend{client: client}, doneTTL, newClaimToken)
}

func mentionClaimKey(botUID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(botUID + "\x00" + idempotencyKey))
	return mentionClaimPrefix + hex.EncodeToString(sum[:])
}

func newClaimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate bot mention claim token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *claimStore) Lookup(key, sha string) (claimOutcome, error) {
	raw, err := s.backend.Get(key)
	if errors.Is(err, errClaimNotFound) {
		return claimOutcome{State: claimMissing}, nil
	}
	if err != nil {
		return claimOutcome{}, fmt.Errorf("lookup bot mention claim: %w", err)
	}
	var record claimRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return claimOutcome{}, fmt.Errorf("decode bot mention claim: %w", err)
	}
	if record.SHA != sha {
		return claimOutcome{State: claimConflict}, nil
	}
	switch record.State {
	case claimRecordPending:
		return claimOutcome{State: claimPending}, nil
	case claimRecordDone:
		if record.EventID <= 0 {
			return claimOutcome{}, errors.New("decode bot mention claim: done record has no event_id")
		}
		return claimOutcome{State: claimReplay, EventID: record.EventID}, nil
	default:
		return claimOutcome{}, fmt.Errorf("decode bot mention claim: unknown state %q", record.State)
	}
}

func (s *claimStore) Begin(key, sha string) (claimOutcome, *claimLease, error) {
	existing, err := s.Lookup(key, sha)
	if err != nil {
		return claimOutcome{}, nil, err
	}
	if existing.State != claimMissing {
		return existing, nil, nil
	}

	token, err := s.newToken()
	if err != nil {
		return claimOutcome{}, nil, err
	}
	pending, err := json.Marshal(claimRecord{State: claimRecordPending, SHA: sha, Token: token})
	if err != nil {
		return claimOutcome{}, nil, fmt.Errorf("encode bot mention claim: %w", err)
	}
	created, err := s.backend.SetNX(key, string(pending), claimPendingTTL)
	if err != nil {
		return claimOutcome{}, nil, fmt.Errorf("reserve bot mention claim: %w", err)
	}
	if !created {
		outcome, lookupErr := s.Lookup(key, sha)
		if lookupErr == nil && outcome.State == claimMissing {
			// The SetNX loser observed the winner's pending claim disappear
			// before the fallback lookup. Treat this benign race as retryable
			// in-progress instead of surfacing an internal error.
			outcome.State = claimPending
		}
		return outcome, nil, lookupErr
	}
	return claimOutcome{State: claimAcquired}, &claimLease{
		key:          key,
		sha:          sha,
		pendingValue: string(pending),
	}, nil
}

func (s *claimStore) Commit(lease *claimLease, event robot.PreparedBotTypedEvent) (bool, error) {
	if lease == nil || event.EventID <= 0 || event.QueueKey == "" || event.Member == "" {
		return false, errors.New("commit bot mention event: invalid lease or prepared event")
	}
	done, err := json.Marshal(claimRecord{State: claimRecordDone, SHA: lease.sha, EventID: event.EventID})
	if err != nil {
		return false, fmt.Errorf("encode committed bot mention claim: %w", err)
	}
	ok, err := s.backend.CompareAndEnqueue(lease.key, lease.pendingValue, string(done), s.doneTTL, event)
	if err != nil {
		return false, fmt.Errorf("commit bot mention event: %w", err)
	}
	return ok, nil
}

func (s *claimStore) Release(lease *claimLease) (bool, error) {
	if lease == nil {
		return false, errors.New("release bot mention claim: nil lease")
	}
	ok, err := s.backend.CompareAndDelete(lease.key, lease.pendingValue)
	if err != nil {
		return false, fmt.Errorf("release bot mention claim: %w", err)
	}
	return ok, nil
}

type redisClaimBackend struct {
	client *rd.Client
}

func (b *redisClaimBackend) Get(key string) (string, error) {
	value, err := b.client.Get(key).Result()
	if err == rd.Nil {
		return "", errClaimNotFound
	}
	return value, err
}

func (b *redisClaimBackend) SetNX(key, value string, ttl time.Duration) (bool, error) {
	return b.client.SetNX(key, value, ttl).Result()
}

var compareAndEnqueueClaimScript = rd.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	redis.call("zadd", KEYS[2], ARGV[4], ARGV[5])
	redis.call("pexpire", KEYS[2], ARGV[3])
	redis.call("psetex", KEYS[1], ARGV[3], ARGV[2])
	return 1
end
return 0
`)

func (b *redisClaimBackend) CompareAndEnqueue(key, oldValue, newValue string, ttl time.Duration, event robot.PreparedBotTypedEvent) (bool, error) {
	result, err := compareAndEnqueueClaimScript.Run(
		b.client,
		[]string{key, event.QueueKey},
		oldValue,
		newValue,
		ttl.Milliseconds(),
		event.EventID,
		event.Member,
	).Int64()
	return result == 1, err
}

var compareAndDeleteClaimScript = rd.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

func (b *redisClaimBackend) CompareAndDelete(key, oldValue string) (bool, error) {
	result, err := compareAndDeleteClaimScript.Run(b.client, []string{key}, oldValue).Int64()
	return result == 1, err
}
