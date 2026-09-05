package message

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	conversationExtraLockKeyPrefix = "conversation_extra:lock:"
	conversationExtraLockTTL       = 10 * time.Second
	conversationExtraLockWait      = 2 * time.Second
	conversationExtraLockRetry     = 40 * time.Millisecond
)

var (
	errConversationExtraLockTimeout = errors.New("conversation extra lock acquisition timed out")
	errConversationExtraLockLost    = errors.New("conversation extra lock ownership lost")
	errConversationExtraVersion     = errors.New("conversation extra version was not advanced")

	releaseConversationExtraLockScript = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)
	renewConversationExtraLockScript = rd.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)
)

// conversationExtraLock serializes every conversation-extra write for one
// user. The synchronization cursor is user-scoped, so a per-channel lock would
// still allow two channels for the same user to commit versions out of order.
type conversationExtraLock struct {
	client *rd.Client
}

type conversationExtraLease struct {
	lock  *conversationExtraLock
	key   string
	owner string
}

func newConversationExtraLock(ctx *config.Context) *conversationExtraLock {
	return &conversationExtraLock{
		client: octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
			o.MaxRetries = 3
			o.ReadTimeout = 3 * time.Second
			o.WriteTimeout = 3 * time.Second
			o.DialTimeout = 3 * time.Second
		}),
	}
}

func conversationExtraLockKey(uid string) string {
	return conversationExtraLockKeyPrefix + uid
}

// Acquire waits for a bounded period and stores a request-unique UUID owner
// together with the lease TTL. A crashed process therefore cannot leave a
// permanent lock behind.
func (l *conversationExtraLock) Acquire(ctx context.Context, uid string) (*conversationExtraLease, error) {
	ownerUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate conversation extra lock owner: %w", err)
	}
	lease := &conversationExtraLease{
		lock:  l,
		key:   conversationExtraLockKey(uid),
		owner: ownerUUID.String(),
	}

	acquireCtx, cancel := context.WithTimeout(ctx, conversationExtraLockWait)
	defer cancel()
	retry := time.NewTicker(conversationExtraLockRetry)
	defer retry.Stop()

	for {
		acquired, err := l.client.WithContext(acquireCtx).
			SetNX(lease.key, lease.owner, conversationExtraLockTTL).
			Result()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("acquire conversation extra lock: %w", ctx.Err())
			}
			if acquireCtx.Err() != nil {
				return nil, errConversationExtraLockTimeout
			}
			return nil, fmt.Errorf("acquire conversation extra lock: %w", err)
		}
		if acquired {
			return lease, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire conversation extra lock: %w", ctx.Err())
		case <-acquireCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("acquire conversation extra lock: %w", ctx.Err())
			}
			return nil, errConversationExtraLockTimeout
		case <-retry.C:
		}
	}
}

// Renew atomically verifies the UUID owner and refreshes the 10-second lease.
// Callers invoke it immediately before the protected database write.
func (l *conversationExtraLease) Renew(ctx context.Context) error {
	result, err := renewConversationExtraLockScript.Run(
		l.lock.client.WithContext(ctx),
		[]string{l.key},
		l.owner,
		conversationExtraLockTTL.Milliseconds(),
	).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return fmt.Errorf("renew conversation extra lock: %w", err)
	}
	if renewed, ok := result.(int64); !ok || renewed != 1 {
		return errConversationExtraLockLost
	}
	return nil
}

// Release uses compare-and-delete so an expired owner can never delete a lock
// subsequently acquired by another request.
func (l *conversationExtraLease) Release(ctx context.Context) error {
	_, err := releaseConversationExtraLockScript.Run(
		l.lock.client.WithContext(ctx),
		[]string{l.key},
		l.owner,
	).Result()
	if err != nil && !errors.Is(err, rd.Nil) {
		return fmt.Errorf("release conversation extra lock: %w", err)
	}
	return nil
}

// withConversationExtraLease keeps lock ownership and release policy in one
// place. CMD delivery is deliberately performed by callers after this method
// returns, so network notification latency never extends the critical section.
func (co *Conversation) withConversationExtraLease(
	ctx context.Context,
	uid string,
	channelID string,
	channelType uint8,
	fn func(*conversationExtraLease) error,
) (err error) {
	lease, err := co.conversationExtraLock.Acquire(ctx, uid)
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if releaseErr := lease.Release(releaseCtx); releaseErr != nil {
			co.Error("释放会话扩展分布式锁失败！",
				zap.Error(releaseErr),
				zap.String("channel_id", channelID),
				zap.Uint8("channel_type", channelType),
			)
		}
	}()
	return fn(lease)
}

// nextConversationExtraVersionLocked combines the existing sequence source
// with the persisted per-user high-water mark. GenSeq reserves process-local
// blocks, so max+1 prevents a different process's older block from moving this
// user's synchronization cursor backwards.
func (co *Conversation) nextConversationExtraVersionLocked(uid string) (int64, error) {
	maxVersion, err := co.conversationExtraDB.maxVersion(uid)
	if err != nil {
		return 0, err
	}
	generated, err := co.ctx.GenSeq(common.SyncConversationExtraKey)
	if err != nil {
		return 0, err
	}
	if generated > maxVersion {
		return generated, nil
	}
	if maxVersion == math.MaxInt64 {
		return 0, errors.New("conversation extra version exhausted")
	}
	return maxVersion + 1, nil
}
