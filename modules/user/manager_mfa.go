package user

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonbase "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
)

const (
	managerMFAChallengeTTL = 15 * time.Minute
	managerMFASendCooldown = 60 * time.Second
	managerMFASendLockTTL  = 120 * time.Second
	managerMFAMaxSends     = 5
	managerMFAStateTTL     = managerMFAChallengeTTL
)

var (
	errManagerMFAChallengeInvalid = errors.New("manager MFA challenge invalid or expired")
)

// managerMFAChallenge is deliberately a non-secret snapshot. It contains no
// cleartext password, OTP or issued token; the password field is a digest of
// the stored password hash so a password change invalidates the challenge.
type managerMFAChallenge struct {
	ID                  string `json:"id"`
	UID                 string `json:"uid"`
	Username            string `json:"username"`
	Role                string `json:"role"`
	Email               string `json:"email"`
	PasswordFingerprint string `json:"password_fingerprint"`
	CreatedAt           int64  `json:"created_at"`
	ExpiresAt           int64  `json:"expires_at"`
}

type managerMFASendState struct {
	Attempts   int    `json:"attempts"`
	Status     string `json:"status"`
	AttemptID  string `json:"attempt_id"`
	CooldownAt int64  `json:"cooldown_at"`
	DeadlineAt int64  `json:"deadline_at"`
}

type managerMFASendRateError struct {
	RetryAfter int
	Reason     string
}

func (e *managerMFASendRateError) Error() string {
	return fmt.Sprintf("manager MFA send limited (%s), retry after %ds", e.Reason, e.RetryAfter)
}

type managerMFAService struct {
	client *rd.Client
	now    func() time.Time
}

func newManagerMFAService(ctx *config.Context) *managerMFAService {
	return &managerMFAService{
		client: octoredis.NewInstrumentedClient(ctx.GetConfig(), func(o *rd.Options) {
			o.MaxRetries = 1
			o.PoolSize = 10
		}),
		now: time.Now,
	}
}

func managerMFAChallengeKey(id string) string { return "manager:mfa:challenge:" + id }
func managerMFAActiveKey(uid string) string   { return "manager:mfa:active:" + uid }
func managerMFASendStateKey(id string) string { return "manager:mfa:send-state:" + id }
func managerMFASendLockKey(id string) string  { return "manager:mfa:send-lock:" + id }

func passwordFingerprint(passwordHash string) string {
	digest := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(digest[:])
}

func (s *managerMFAService) createChallenge(ctx context.Context, challenge managerMFAChallenge) error {
	payload, err := json.Marshal(challenge)
	if err != nil {
		return err
	}
	ttl := int(managerMFAChallengeTTL.Seconds())
	err = s.client.WithContext(ctx).Eval(`
local old = redis.call('GET', KEYS[2])
if old and old ~= ARGV[1] then
  redis.call('DEL', ARGV[3] .. old, ARGV[4] .. old, ARGV[5] .. old)
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', ARGV[6])
redis.call('SET', KEYS[2], ARGV[1], 'EX', ARGV[6])
redis.call('DEL', KEYS[3], KEYS[4], KEYS[5])
return 1
`, []string{
		managerMFAChallengeKey(challenge.ID),
		managerMFAActiveKey(challenge.UID),
		managerMFASendStateKey(challenge.ID),
		commonbase.EmailCodeKey(challenge.Email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailCodeStatusKey(challenge.Email, commonbase.CodeTypeManagerLogin),
	}, challenge.ID, string(payload), "manager:mfa:challenge:", "manager:mfa:send-state:", "manager:mfa:send-lock:", ttl).Err()
	return err
}

func (s *managerMFAService) loadChallenge(ctx context.Context, id string) (*managerMFAChallenge, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errManagerMFAChallengeInvalid
	}
	payload, err := s.client.WithContext(ctx).Get(managerMFAChallengeKey(id)).Result()
	if err == rd.Nil {
		return nil, errManagerMFAChallengeInvalid
	}
	if err != nil {
		return nil, err
	}
	var challenge managerMFAChallenge
	if err := json.Unmarshal([]byte(payload), &challenge); err != nil || challenge.ID != id {
		return nil, errManagerMFAChallengeInvalid
	}
	if s.now().UnixMilli() >= challenge.ExpiresAt {
		return nil, errManagerMFAChallengeInvalid
	}
	active, err := s.client.WithContext(ctx).Get(managerMFAActiveKey(challenge.UID)).Result()
	if err != nil {
		if err == rd.Nil {
			return nil, errManagerMFAChallengeInvalid
		}
		return nil, err
	}
	if active != challenge.ID {
		return nil, errManagerMFAChallengeInvalid
	}
	return &challenge, nil
}

func (s *managerMFAService) claimSend(ctx context.Context, challenge *managerMFAChallenge, attemptID string) (int, error) {
	now := s.now().UnixMilli()
	result, err := s.client.WithContext(ctx).Eval(`
if redis.call('EXISTS', KEYS[1]) == 0 or redis.call('GET', KEYS[2]) ~= ARGV[1] then
  return {-1, 0}
end
local deadline = tonumber(ARGV[4])
if tonumber(ARGV[2]) >= deadline then
  return {-2, 0}
end
local lockTTL = redis.call('PTTL', KEYS[6])
if lockTTL > 0 then
  return {-3, math.max(1, math.ceil(lockTTL / 1000))}
end
local attempts = tonumber(redis.call('HGET', KEYS[3], 'attempts') or '0')
if attempts >= tonumber(ARGV[5]) then
  return {-5, 0}
end
local cooldown = tonumber(redis.call('HGET', KEYS[3], 'cooldown_at') or '0')
if cooldown > tonumber(ARGV[2]) then
  return {-4, math.max(1, math.ceil((cooldown - tonumber(ARGV[2])) / 1000))}
end
local claimed = redis.call('SET', KEYS[6], ARGV[3], 'PX', ARGV[7], 'NX')
if not claimed then
  return {-3, math.max(1, math.ceil(redis.call('PTTL', KEYS[6]) / 1000))}
end
attempts = attempts + 1
redis.call('HSET', KEYS[3],
  'attempts', attempts,
  'status', 'pending',
  'attempt_id', ARGV[3],
  'cooldown_at', tonumber(ARGV[2]) + tonumber(ARGV[6]),
  'deadline_at', deadline)
local ttl = math.min(tonumber(ARGV[8]), math.max(1, math.ceil((deadline - tonumber(ARGV[2])) / 1000)))
redis.call('EXPIRE', KEYS[3], ttl)
redis.call('DEL', KEYS[4], KEYS[5])
return {1, 0}
`, []string{
		managerMFAChallengeKey(challenge.ID),
		managerMFAActiveKey(challenge.UID),
		managerMFASendStateKey(challenge.ID),
		commonbase.EmailCodeKey(challenge.Email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailCodeStatusKey(challenge.Email, commonbase.CodeTypeManagerLogin),
		managerMFASendLockKey(challenge.ID),
	}, challenge.ID, now, attemptID, challenge.ExpiresAt, managerMFAMaxSends,
		managerMFASendCooldown.Milliseconds(), managerMFASendLockTTL.Milliseconds(),
		int(managerMFAStateTTL.Seconds())).Result()
	if err != nil {
		return 0, err
	}
	parts, ok := result.([]interface{})
	if !ok || len(parts) != 2 {
		return 0, errors.New("invalid manager MFA send claim result")
	}
	status, ok := redisInt(parts[0])
	if !ok {
		return 0, errors.New("invalid manager MFA send claim status")
	}
	retry, _ := redisInt(parts[1])
	switch status {
	case 1:
		return 0, nil
	case -1, -2:
		return 0, errManagerMFAChallengeInvalid
	case -3:
		return retry, &managerMFASendRateError{RetryAfter: retry, Reason: "in_flight"}
	case -4:
		return retry, &managerMFASendRateError{RetryAfter: retry, Reason: "cooldown"}
	case -5:
		return 0, &managerMFASendRateError{RetryAfter: 0, Reason: "max_attempts"}
	default:
		return 0, errors.New("manager MFA send claim rejected")
	}
}

func (s *managerMFAService) completeSend(ctx context.Context, challengeID, attemptID string, success bool) (bool, error) {
	status := "failed"
	if success {
		status = "sent"
	}
	result, err := s.client.WithContext(ctx).Eval(`
if redis.call('HGET', KEYS[1], 'attempt_id') ~= ARGV[1] or redis.call('HGET', KEYS[1], 'status') ~= 'pending' then
  return 0
end
redis.call('HSET', KEYS[1], 'status', ARGV[2])
if redis.call('GET', KEYS[2]) == ARGV[1] then
  redis.call('DEL', KEYS[2])
end
return 1
`, []string{managerMFASendStateKey(challengeID), managerMFASendLockKey(challengeID)}, attemptID, status).Result()
	if err != nil {
		return false, err
	}
	value, ok := redisInt(result)
	return ok && value == 1, nil
}

// invalidateUID removes the active challenge and all of its send state. The
// caller supplies the old/new email values so the shared manager OTP keys are
// cleared as well; account snapshot validation remains the authoritative
// guard if a new challenge races with this cleanup.
func (s *managerMFAService) invalidateUID(ctx context.Context, uid string, emails ...string) error {
	activeKey := managerMFAActiveKey(uid)
	active, err := s.client.WithContext(ctx).Get(activeKey).Result()
	if err != nil && err != rd.Nil {
		return err
	}
	if active != "" {
		if err := s.client.WithContext(ctx).Del(
			activeKey,
			managerMFAChallengeKey(active),
			managerMFASendStateKey(active),
			managerMFASendLockKey(active),
		).Err(); err != nil {
			return err
		}
	}
	for _, email := range emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		if err := s.client.WithContext(ctx).Del(
			commonbase.EmailCodeKey(email, commonbase.CodeTypeManagerLogin),
			commonbase.EmailCodeStatusKey(email, commonbase.CodeTypeManagerLogin),
		).Err(); err != nil {
			return err
		}
	}
	return nil
}

func redisInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	case []byte:
		var n int
		_, err := fmt.Sscanf(string(v), "%d", &n)
		return n, err == nil
	default:
		return 0, false
	}
}

// managerMFAResponseError centralizes the two new status-preserving errors.
// The old manager login endpoint continues to use the legacy 400 envelope,
// while these newly introduced unauthenticated endpoints expose 503/429.
func managerMFAResponseError(c *wkhttp.Context, code codes.Code, details i18n.Details) {
	httperr.ResponseErrorLWithStatus(c, code, nil, details)
}

func managerMFAServiceUnavailable(c *wkhttp.Context, code codes.Code) {
	managerMFAResponseError(c, code, nil)
}
