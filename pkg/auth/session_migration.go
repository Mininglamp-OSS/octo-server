package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	rd "github.com/go-redis/redis"
)

var (
	migrateLegacyTokenScript = rd.NewScript(`
local value = redis.call("GET", KEYS[1])
if not value then
  return {0, -2, 0}
end
local ttl = redis.call("PTTL", KEYS[1])
local version = 1
if string.sub(value, 1, 3) == "v3:" then
  version = 3
elseif string.sub(value, 1, 3) == "v2:" then
  version = 2
end
if version == 3 then
  if ttl <= 0 then
    return {version, ttl, 2}
  end
  return {version, ttl, 0}
end
if ttl == -2 then
  return {0, ttl, 0}
end
if ttl ~= -1 and ttl <= 0 then
  return {version, ttl, 2}
end
local max_target = tonumber(ARGV[2]) + tonumber(ARGV[3])
local target = max_target
if ttl == -1 or ARGV[5] == "cap" then
  target = tonumber(ARGV[1])
  if max_target < target then
    target = max_target
  end
end
local target_ttl = target - tonumber(ARGV[2])
if target_ttl <= 0 then
  if ARGV[4] == "1" and ARGV[6] ~= "1" then
    return {version, ttl, 4}
  end
  if ARGV[4] == "1" then
    redis.call("DEL", KEYS[1])
  end
  return {version, ttl, 3}
end
local shorten = ttl == -1 or ttl > target_ttl
if not shorten then
  return {version, ttl, 0}
end
if ARGV[4] == "1" then
  redis.call("PEXPIREAT", KEYS[1], target)
end
return {version, ttl, 1}
`)
	renewMigrationLockScript = rd.NewScript(`
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
  return 0
end
return redis.call("PEXPIRE", KEYS[1], ARGV[2])
`)
)

var (
	ErrMigrationLockHeld                          = errors.New("auth: token migration lock is held")
	ErrMigrationLockLost                          = errors.New("auth: token migration lock was lost")
	ErrMigrationElapsedCutoffConfirmationRequired = errors.New("auth: migration apply requires confirm elapsed cutoff")
)

type LegacyMigrationOptions struct {
	CampaignID           string
	CutoffAt             time.Time
	FinitePolicy         LegacyFinitePolicy
	BatchSize            int64
	Interval             time.Duration
	Apply                bool
	ConfirmElapsedCutoff bool
	Lease                time.Duration
}

type LegacyFinitePolicy string

const (
	// LegacyFinitePolicyNatural preserves finite legacy deadlines that already
	// fit within maxTTL. Persistent and over-max records are still bounded.
	LegacyFinitePolicyNatural LegacyFinitePolicy = "natural"
	// LegacyFinitePolicyCap also compresses finite legacy deadlines to the
	// campaign cutoff. This may log users out earlier and requires approval.
	LegacyFinitePolicyCap LegacyFinitePolicy = "cap"
)

type LegacyMigrationResult struct {
	Complete    bool   `json:"complete"`
	CampaignID  string `json:"campaign_id"`
	Scanned     int64  `json:"scanned"`
	Missing     int64  `json:"missing"`
	V1          int64  `json:"v1"`
	V2          int64  `json:"v2"`
	V3          int64  `json:"v3"`
	Shortened   int64  `json:"shortened"`
	WouldDelete int64  `json:"would_delete"`
	Deleted     int64  `json:"deleted"`
	Unchanged   int64  `json:"unchanged"`
	Invalid     int64  `json:"invalid"`
	V3NonFinite int64  `json:"v3_non_finite"`
	LastCursor  uint64 `json:"last_cursor"`
	LockLost    bool   `json:"lock_lost"`
}

type legacyMigrationCampaign struct {
	CampaignID   string `json:"campaign_id"`
	CutoffAtMS   int64  `json:"cutoff_at_unix_ms"`
	MaxTTLMS     int64  `json:"max_ttl_ms"`
	FinitePolicy string `json:"finite_policy"`
}

type legacyMigrationCheckpoint struct {
	CampaignID      string                `json:"campaign_id"`
	RedisInstanceID string                `json:"redis_instance_id"`
	Cursor          uint64                `json:"cursor"`
	Result          LegacyMigrationResult `json:"result"`
}

func (s *RedisSessionStore) MigrateLegacySessions(ctx context.Context, options LegacyMigrationOptions) (result LegacyMigrationResult, err error) {
	campaign, err := s.validateMigrationOptions(options)
	if err != nil {
		return LegacyMigrationResult{}, err
	}
	result.CampaignID = campaign.CampaignID
	if options.Apply {
		control, err := s.RolloutControl(ctx)
		if err != nil {
			return result, err
		}
		if control == nil || control.ModeFloor.rank() < SessionModeRevoke.rank() {
			return result, errors.New("auth: migration apply requires persisted revoke rollout floor")
		}
	}

	owner := ""
	redisInstanceID := ""
	var lockErrors <-chan error
	if options.Apply {
		if err := s.ensureMigrationCampaign(campaign); err != nil {
			return result, err
		}
		owner, err = newSessionGeneration()
		if err != nil {
			return result, err
		}
		acquired, err := s.client.SetNX(s.migrationLockKey(), owner, options.Lease).Result()
		if err != nil {
			return result, fmt.Errorf("auth: acquire migration lock: %w", err)
		}
		if !acquired {
			return result, ErrMigrationLockHeld
		}
		defer func() {
			_, _ = compareDeleteScript.Run(s.client, []string{s.migrationLockKey()}, owner).Result()
		}()
		keepaliveCtx, stopKeepalive := context.WithCancel(ctx)
		defer stopKeepalive()
		lockErrors = s.keepMigrationLockAlive(keepaliveCtx, owner, options.Lease)
		redisInstanceID, err = s.currentRedisInstanceID()
		if err != nil {
			return result, fmt.Errorf("auth: identify migration Redis instance: %w", err)
		}
		checkpoint, loadErr := s.loadMigrationCheckpoint(campaign.CampaignID)
		if loadErr != nil {
			return result, loadErr
		}
		if checkpoint != nil && !checkpoint.Result.Complete {
			result = checkpoint.Result
			result.Complete = false
			result.LockLost = false
			if checkpoint.RedisInstanceID != redisInstanceID {
				result.LastCursor = 0
			}
		}
	}

	var throttle <-chan time.Time
	var ticker *time.Ticker
	if options.Interval > 0 {
		ticker = time.NewTicker(options.Interval)
		defer ticker.Stop()
		throttle = ticker.C
	}
	cursor := result.LastCursor
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if options.Apply {
			if err := migrationLockError(lockErrors); err != nil {
				result.LockLost = errors.Is(err, ErrMigrationLockLost)
				return result, err
			}
			if err := s.confirmMigrationLock(owner, options.Lease); err != nil {
				result.LockLost = errors.Is(err, ErrMigrationLockLost)
				return result, err
			}
			currentRedisInstanceID, err := s.currentRedisInstanceID()
			if err != nil {
				return result, fmt.Errorf("auth: identify migration Redis instance before scan: %w", err)
			}
			if currentRedisInstanceID != redisInstanceID {
				redisInstanceID = currentRedisInstanceID
				cursor = 0
				result.LastCursor = 0
				if err := s.confirmMigrationLock(owner, options.Lease); err != nil {
					result.LockLost = errors.Is(err, ErrMigrationLockLost)
					return result, err
				}
			}
		}
		keys, next, err := s.client.Scan(cursor, s.tokenPrefix+"*", options.BatchSize).Result()
		if err != nil {
			return result, fmt.Errorf("auth: migration scan: %w", err)
		}
		for _, key := range keys {
			if options.Apply {
				if err := migrationLockError(lockErrors); err != nil {
					result.LockLost = errors.Is(err, ErrMigrationLockLost)
					return result, err
				}
			}
			if throttle != nil {
				select {
				case <-ctx.Done():
					return result, ctx.Err()
				case <-throttle:
				}
			}
			if options.Apply {
				if err := migrationLockError(lockErrors); err != nil {
					result.LockLost = errors.Is(err, ErrMigrationLockLost)
					return result, err
				}
			}
			if err := s.migrateLegacyToken(key, campaign, options.Apply, options.ConfirmElapsedCutoff, &result); err != nil {
				return result, err
			}
		}
		cursor = next
		result.LastCursor = cursor
		if options.Apply {
			currentRedisInstanceID, err := s.currentRedisInstanceID()
			if err != nil {
				return result, fmt.Errorf("auth: identify migration Redis instance after scan: %w", err)
			}
			if currentRedisInstanceID != redisInstanceID {
				redisInstanceID = currentRedisInstanceID
				cursor = 0
				result.LastCursor = 0
				if err := s.confirmMigrationLock(owner, options.Lease); err != nil {
					result.LockLost = errors.Is(err, ErrMigrationLockLost)
					return result, err
				}
				if err := s.saveMigrationCheckpoint(campaign.CampaignID, redisInstanceID, 0, result); err != nil {
					return result, err
				}
				continue
			}
			if err := s.confirmMigrationLock(owner, options.Lease); err != nil {
				result.LockLost = errors.Is(err, ErrMigrationLockLost)
				return result, err
			}
			if err := s.saveMigrationCheckpoint(campaign.CampaignID, redisInstanceID, cursor, result); err != nil {
				return result, err
			}
		}
		if cursor == 0 {
			if options.Apply {
				currentRedisInstanceID, err := s.currentRedisInstanceID()
				if err != nil {
					return result, fmt.Errorf("auth: identify migration Redis instance before completion: %w", err)
				}
				if currentRedisInstanceID != redisInstanceID {
					redisInstanceID = currentRedisInstanceID
					result.LastCursor = 0
					if err := s.confirmMigrationLock(owner, options.Lease); err != nil {
						result.LockLost = errors.Is(err, ErrMigrationLockLost)
						return result, err
					}
					if err := s.saveMigrationCheckpoint(campaign.CampaignID, redisInstanceID, 0, result); err != nil {
						return result, err
					}
					continue
				}
			}
			result.Complete = true
			if options.Apply {
				if err := s.saveMigrationCheckpoint(campaign.CampaignID, redisInstanceID, 0, result); err != nil {
					return result, err
				}
				if err := s.recordMigrationCompletion(campaign.CampaignID, redisInstanceID, result); err != nil {
					return result, err
				}
			}
			return result, nil
		}
	}
}

func (s *RedisSessionStore) validateMigrationOptions(options LegacyMigrationOptions) (legacyMigrationCampaign, error) {
	campaignID := strings.TrimSpace(options.CampaignID)
	if campaignID == "" || len(campaignID) > 64 {
		return legacyMigrationCampaign{}, errors.New("auth: migration campaign id must contain 1-64 characters")
	}
	if options.CutoffAt.IsZero() {
		return legacyMigrationCampaign{}, errors.New("auth: migration cutoff is required")
	}
	if options.FinitePolicy != LegacyFinitePolicyNatural && options.FinitePolicy != LegacyFinitePolicyCap {
		return legacyMigrationCampaign{}, errors.New("auth: migration finite policy must be natural or cap")
	}
	if options.BatchSize <= 0 || options.BatchSize > 10_000 {
		return legacyMigrationCampaign{}, errors.New("auth: migration batch size must be between 1 and 10000")
	}
	if options.Interval < 0 {
		return legacyMigrationCampaign{}, errors.New("auth: migration interval must not be negative")
	}
	if options.Apply {
		if options.CutoffAt.UTC().UnixMilli() <= s.now().UTC().UnixMilli() && !options.ConfirmElapsedCutoff {
			return legacyMigrationCampaign{}, ErrMigrationElapsedCutoffConfirmationRequired
		}
		if options.Lease < 5*time.Second || options.Lease > 5*time.Minute {
			return legacyMigrationCampaign{}, errors.New("auth: migration lease must be between 5s and 5m")
		}
	}
	return legacyMigrationCampaign{
		CampaignID:   campaignID,
		CutoffAtMS:   options.CutoffAt.UTC().UnixMilli(),
		MaxTTLMS:     s.maxTTL.Milliseconds(),
		FinitePolicy: string(options.FinitePolicy),
	}, nil
}

func (s *RedisSessionStore) migrateLegacyToken(key string, campaign legacyMigrationCampaign, apply, confirmElapsedCutoff bool, result *LegacyMigrationResult) error {
	nowMS := s.now().UTC().UnixMilli()
	applyArg := "0"
	if apply {
		applyArg = "1"
	}
	confirmElapsedCutoffArg := "0"
	if confirmElapsedCutoff {
		confirmElapsedCutoffArg = "1"
	}
	raw, err := migrateLegacyTokenScript.Run(
		s.client,
		[]string{key},
		campaign.CutoffAtMS,
		nowMS,
		campaign.MaxTTLMS,
		applyArg,
		campaign.FinitePolicy,
		confirmElapsedCutoffArg,
	).Result()
	if err != nil {
		return fmt.Errorf("auth: migrate token record: %w", err)
	}
	parts, ok := raw.([]interface{})
	if !ok || len(parts) != 3 {
		return fmt.Errorf("auth: invalid migration result %T", raw)
	}
	version, err := redisInteger(parts[0])
	if err != nil {
		return err
	}
	ttl, err := redisInteger(parts[1])
	if err != nil {
		return err
	}
	action, err := redisInteger(parts[2])
	if err != nil {
		return err
	}
	result.Scanned++
	switch version {
	case 0:
		result.Missing++
	case 1:
		result.V1++
	case 2:
		result.V2++
	case 3:
		result.V3++
		if ttl <= 0 {
			result.V3NonFinite++
		}
	default:
		result.Invalid++
	}
	switch action {
	case 0:
		result.Unchanged++
	case 1:
		result.Shortened++
	case 2:
		result.Invalid++
	case 3:
		if apply {
			result.Deleted++
		} else {
			result.WouldDelete++
		}
	case 4:
		return ErrMigrationElapsedCutoffConfirmationRequired
	default:
		return fmt.Errorf("auth: invalid migration action %d", action)
	}
	return nil
}

func (s *RedisSessionStore) ensureMigrationCampaign(campaign legacyMigrationCampaign) error {
	encoded, err := json.Marshal(campaign)
	if err != nil {
		return err
	}
	key := s.migrationCampaignKey(campaign.CampaignID)
	current, err := s.client.Get(key).Result()
	if err == nil {
		return s.validatePersistedMigrationCampaign(key, current, string(encoded))
	}
	if err != rd.Nil {
		return fmt.Errorf("auth: load migration campaign: %w", err)
	}
	if campaign.CutoffAtMS <= s.now().UTC().UnixMilli() {
		return errors.New("auth: a new migration campaign cutoff must be in the future")
	}
	created, err := s.client.SetNX(key, string(encoded), 0).Result()
	if err != nil {
		return fmt.Errorf("auth: initialize migration campaign: %w", err)
	}
	if created {
		return nil
	}
	current, err = s.client.Get(key).Result()
	if err != nil {
		return fmt.Errorf("auth: load migration campaign: %w", err)
	}
	return s.validatePersistedMigrationCampaign(key, current, string(encoded))
}

func (s *RedisSessionStore) validatePersistedMigrationCampaign(key, current, expected string) error {
	if current != expected {
		return errors.New("auth: migration campaign parameters do not match persisted campaign")
	}
	if ttl, err := s.client.PTTL(key).Result(); err != nil || ttl != -time.Millisecond {
		return errors.New("auth: migration campaign record must not expire")
	}
	return nil
}

func (s *RedisSessionStore) loadMigrationCheckpoint(campaignID string) (*legacyMigrationCheckpoint, error) {
	raw, err := s.client.Get(s.migrationCheckpointKey(campaignID)).Result()
	if err == rd.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: load migration checkpoint: %w", err)
	}
	var checkpoint legacyMigrationCheckpoint
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		return nil, fmt.Errorf("auth: decode migration checkpoint: %w", err)
	}
	if checkpoint.CampaignID != campaignID {
		return nil, errors.New("auth: migration checkpoint belongs to another campaign")
	}
	if checkpoint.Result.CampaignID != campaignID || checkpoint.Cursor != checkpoint.Result.LastCursor {
		return nil, errors.New("auth: migration checkpoint is internally inconsistent")
	}
	return &checkpoint, nil
}

func (s *RedisSessionStore) saveMigrationCheckpoint(campaignID, redisInstanceID string, cursor uint64, result LegacyMigrationResult) error {
	encoded, err := json.Marshal(legacyMigrationCheckpoint{
		CampaignID:      campaignID,
		RedisInstanceID: redisInstanceID,
		Cursor:          cursor,
		Result:          result,
	})
	if err != nil {
		return err
	}
	if err := s.client.Set(s.migrationCheckpointKey(campaignID), string(encoded), 0).Err(); err != nil {
		return fmt.Errorf("auth: save migration checkpoint: %w", err)
	}
	return nil
}

func (s *RedisSessionStore) currentRedisInstanceID() (string, error) {
	if s.redisInstanceIDFn != nil {
		instanceID, err := s.redisInstanceIDFn()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(instanceID) == "" {
			return "", errors.New("Redis instance identity must not be empty")
		}
		return instanceID, nil
	}
	info, err := s.client.Info("server").Result()
	if err != nil {
		return "", fmt.Errorf("read Redis INFO server: %w", err)
	}
	var runID string
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "run_id:") {
			runID = strings.TrimSpace(strings.TrimPrefix(line, "run_id:"))
			break
		}
	}
	if runID == "" {
		return "", errors.New("Redis INFO server did not return run_id")
	}
	sum := sha256.Sum256([]byte("token-session-migration/v1:" + runID))
	return fmt.Sprintf("%x", sum[:]), nil
}

func (s *RedisSessionStore) renewMigrationLock(owner string, lease time.Duration) (bool, error) {
	result, err := renewMigrationLockScript.Run(s.client, []string{s.migrationLockKey()}, owner, lease.Milliseconds()).Result()
	if err != nil {
		return false, fmt.Errorf("auth: renew migration lock: %w", err)
	}
	value, err := redisInteger(result)
	return value == 1, err
}

func (s *RedisSessionStore) confirmMigrationLock(owner string, lease time.Duration) error {
	ok, err := s.renewMigrationLock(owner, lease)
	if err != nil {
		return err
	}
	if !ok {
		return ErrMigrationLockLost
	}
	return nil
}

func (s *RedisSessionStore) keepMigrationLockAlive(ctx context.Context, owner string, lease time.Duration) <-chan error {
	errorsCh := make(chan error, 1)
	interval := lease / 3
	if interval > time.Second {
		interval = time.Second
	}
	go func() {
		defer close(errorsCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.confirmMigrationLock(owner, lease); err != nil {
					errorsCh <- err
					return
				}
			}
		}
	}()
	return errorsCh
}

func migrationLockError(errorsCh <-chan error) error {
	if errorsCh == nil {
		return nil
	}
	select {
	case err, ok := <-errorsCh:
		if !ok {
			return nil
		}
		return err
	default:
		return nil
	}
}

func (s *RedisSessionStore) migrationCampaignKey(campaignID string) string {
	return s.uidTokenPrefix + "auth:migration:campaign:" + sessionSubject(campaignID)
}

func (s *RedisSessionStore) migrationCheckpointKey(campaignID string) string {
	return s.uidTokenPrefix + "auth:migration:checkpoint:" + sessionSubject(campaignID)
}

func (s *RedisSessionStore) migrationLockKey() string {
	return s.uidTokenPrefix + "auth:migration:lock"
}
