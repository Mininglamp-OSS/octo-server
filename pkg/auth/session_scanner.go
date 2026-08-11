package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const rolloutScanLeaseTTL = 30 * time.Second

var ErrRolloutScanLeaseHeld = errors.New("auth: session rollout scan lease is held")

// instanceBoundScanner guarantees that one cursor and its aggregate evidence
// belong to one Redis run_id. A failover returns restarted=true with cursor 0;
// callers must reset their counters before continuing.
type instanceBoundScanner struct {
	store      *RedisSessionStore
	instanceID string
}

func newInstanceBoundScanner(store *RedisSessionStore) (*instanceBoundScanner, error) {
	instanceID, err := store.currentRedisInstanceID()
	if err != nil {
		return nil, err
	}
	return &instanceBoundScanner{store: store, instanceID: instanceID}, nil
}

func (s *instanceBoundScanner) Scan(
	ctx context.Context,
	cursor uint64,
	pattern string,
	count int64,
) (keys []string, next uint64, restarted bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	if restarted, err := s.Check(ctx); err != nil {
		return nil, 0, false, fmt.Errorf("auth: identify Redis instance before scan: %w", err)
	} else if restarted {
		return nil, 0, true, nil
	}
	keys, next, err = s.store.client.Scan(cursor, pattern, count).Result()
	if err != nil {
		return nil, 0, false, err
	}
	if restarted, err := s.Check(ctx); err != nil {
		return nil, 0, false, fmt.Errorf("auth: identify Redis instance after scan: %w", err)
	} else if restarted {
		return nil, 0, true, nil
	}
	return keys, next, false, nil
}

func (s *instanceBoundScanner) InstanceID() string { return s.instanceID }

// Check binds per-key work after SCAN to the same Redis process. Callers must
// invoke it before accepting a batch or marking a scan complete; SCAN's own
// after-check cannot see a failover while returned keys are being processed.
func (s *instanceBoundScanner) Check(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := s.store.currentRedisInstanceID()
	if err != nil {
		return false, err
	}
	if current == s.instanceID {
		return false, nil
	}
	s.instanceID = current
	return true, nil
}

type rolloutScanLease struct {
	store  *RedisSessionStore
	owner  string
	lease  time.Duration
	errors <-chan error
	stop   context.CancelFunc
}

func (s *RedisSessionStore) acquireRolloutScanLease(ctx context.Context, lease time.Duration) (*rolloutScanLease, error) {
	if lease < 5*time.Second {
		lease = rolloutScanLeaseTTL
	}
	owner, err := newSessionGeneration()
	if err != nil {
		return nil, err
	}
	acquired, err := s.client.SetNX(s.rolloutScanLeaseKey(), owner, lease).Result()
	if err != nil {
		return nil, fmt.Errorf("auth: acquire rollout scan lease: %w", err)
	}
	if !acquired {
		return nil, ErrRolloutScanLeaseHeld
	}
	keepaliveCtx, stop := context.WithCancel(ctx)
	errors := s.keepRedisLeaseAlive(keepaliveCtx, s.rolloutScanLeaseKey(), owner, lease)
	return &rolloutScanLease{store: s, owner: owner, lease: lease, errors: errors, stop: stop}, nil
}

// stop is stored separately from context so Release can end the keepalive
// before deleting the key.
func (l *rolloutScanLease) Release() {
	if l == nil {
		return
	}
	l.stop()
	_, _ = compareDeleteScript.Run(l.store.client, []string{l.store.rolloutScanLeaseKey()}, l.owner).Result()
}

func (l *rolloutScanLease) Err() error {
	if l == nil {
		return nil
	}
	select {
	case err, ok := <-l.errors:
		if !ok {
			return nil
		}
		if err != nil {
			return fmt.Errorf("auth: rollout scan lease lost: %w", err)
		}
	default:
	}
	return nil
}

// Confirm synchronously proves ownership at a batch boundary. The keepalive
// reports background failures, while this closes the final-batch window in
// which a deleted or rolled-back lease could otherwise yield complete evidence.
func (l *rolloutScanLease) Confirm() error {
	if l == nil {
		return errors.New("auth: rollout scan lease is unavailable")
	}
	if err := l.Err(); err != nil {
		return err
	}
	result, err := renewMigrationLockScript.Run(
		l.store.client,
		[]string{l.store.rolloutScanLeaseKey()},
		l.owner,
		l.lease.Milliseconds(),
	).Result()
	if err != nil {
		return fmt.Errorf("auth: confirm rollout scan lease: %w", err)
	}
	ok, err := redisInteger(result)
	if err != nil {
		return fmt.Errorf("auth: confirm rollout scan lease: %w", err)
	}
	if ok != 1 {
		return fmt.Errorf("auth: rollout scan lease lost: %w", ErrMigrationLockLost)
	}
	return nil
}

func (s *RedisSessionStore) keepRedisLeaseAlive(
	ctx context.Context,
	key, owner string,
	lease time.Duration,
) <-chan error {
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		interval := lease / 3
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				result, err := renewMigrationLockScript.Run(
					s.client, []string{key}, owner, lease.Milliseconds(),
				).Result()
				if err != nil {
					errs <- err
					return
				}
				ok, err := redisInteger(result)
				if err != nil || ok != 1 {
					if err == nil {
						err = ErrMigrationLockLost
					}
					errs <- err
					return
				}
			}
		}
	}()
	return errs
}

func (s *RedisSessionStore) rolloutScanLeaseKey() string {
	return s.uidTokenPrefix + "auth:rollout-scan-owner"
}
