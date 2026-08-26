package user

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	commonbase "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newManagerMFAStateService(t *testing.T) *managerMFAService {
	t.Helper()
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg, func(options *rd.Options) {
		options.MaxRetries = 1
	})
	require.NoError(t, client.Ping().Err())
	t.Cleanup(func() { _ = client.Close() })
	return &managerMFAService{client: client, now: time.Now}
}

func TestMaskManagerEmailPreservesDomainAndMasksLocalPart(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "normal", input: "alice@example.com", want: "axxxxe@example.com"},
		{name: "two local characters", input: "ab@example.com", want: "axxxxb@example.com"},
		{name: "one local character", input: "a@example.com", want: "axxxx@example.com"},
		{name: "normalizes case and whitespace", input: " Alice@Example.COM ", want: "axxxxe@example.com"},
		{name: "invalid address is unchanged", input: "not-an-email", want: "not-an-email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, maskManagerEmail(tt.input))
		})
	}
}

func TestManagerMFAChallengeReplacesOldChallengeAndHonorsAbsoluteDeadline(t *testing.T) {
	service := newManagerMFAStateService(t)
	ctx := context.Background()
	uid := "mfa-state-uid-" + time.Now().Format("150405.000000000")
	firstID := uid + "-first"
	secondID := uid + "-second"
	firstEmail := uid + "-first@example.com"
	secondEmail := uid + "-second@example.com"
	base := time.Now()

	first := managerMFAChallenge{
		ID: firstID, UID: uid, Username: uid, Role: "superAdmin", Email: firstEmail,
		PasswordFingerprint: "fingerprint-1", CreatedAt: base.UnixMilli(),
		ExpiresAt: base.Add(managerMFAChallengeTTL).UnixMilli(),
	}
	second := first
	second.ID = secondID
	second.Email = secondEmail

	require.NoError(t, service.createChallenge(ctx, first))
	require.NoError(t, service.createChallenge(ctx, second))
	_, err := service.loadChallenge(ctx, firstID)
	assert.ErrorIs(t, err, errManagerMFAChallengeInvalid)
	loaded, err := service.loadChallenge(ctx, secondID)
	require.NoError(t, err)
	assert.Equal(t, secondID, loaded.ID)

	service.now = func() time.Time { return base.Add(managerMFAChallengeTTL) }
	_, err = service.loadChallenge(ctx, secondID)
	assert.ErrorIs(t, err, errManagerMFAChallengeInvalid,
		"the absolute deadline must invalidate a key even before Redis TTL cleanup")
}

func TestManagerMFASendClaimEnforcesCooldownAndMaximum(t *testing.T) {
	service := newManagerMFAStateService(t)
	ctx := context.Background()
	uid := "mfa-send-uid-" + time.Now().Format("150405.000000000")
	challengeID := uid + "-challenge"
	base := time.Now()
	challenge := managerMFAChallenge{
		ID: challengeID, UID: uid, Username: uid, Role: "superAdmin", Email: uid + "@example.com",
		PasswordFingerprint: "fingerprint", CreatedAt: base.UnixMilli(),
		ExpiresAt: base.Add(managerMFAChallengeTTL).UnixMilli(),
	}
	require.NoError(t, service.createChallenge(ctx, challenge))

	// A new send claim invalidates the previous code before any SMTP I/O.
	codeKey := commonbase.EmailCodeKey(challenge.Email, commonbase.CodeTypeManagerLogin)
	statusKey := commonbase.EmailCodeStatusKey(challenge.Email, commonbase.CodeTypeManagerLogin)
	require.NoError(t, service.client.Set(codeKey, "111111", time.Minute).Err())
	require.NoError(t, service.client.Set(statusKey, "sent:old", time.Minute).Err())
	current := base
	service.now = func() time.Time { return current }
	_, err := service.claimSend(ctx, &challenge, "attempt-1")
	require.NoError(t, err)
	oldCode, getErr := service.client.Get(codeKey).Result()
	require.ErrorIs(t, getErr, rd.Nil)
	assert.Empty(t, oldCode)
	oldStatus, getErr := service.client.Get(statusKey).Result()
	require.ErrorIs(t, getErr, rd.Nil)
	assert.Empty(t, oldStatus)
	_, err = service.claimSend(ctx, &challenge, "attempt-in-flight")
	var limited *managerMFASendRateError
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, "in_flight", limited.Reason)
	_, err = service.completeSend(ctx, challengeID, "attempt-1", false)
	require.NoError(t, err)

	current = base.Add(30 * time.Second)
	_, err = service.claimSend(ctx, &challenge, "attempt-cooldown")
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, "cooldown", limited.Reason)

	// Advance the test clock beyond each cooldown without waiting in real time.
	for attempt := 2; attempt <= managerMFAMaxSends; attempt++ {
		current = base.Add(time.Duration(attempt-1) * (managerMFASendCooldown + time.Second))
		_, err = service.claimSend(ctx, &challenge, "attempt-"+string(rune('0'+attempt)))
		require.NoError(t, err)
		_, err = service.completeSend(ctx, challengeID, "attempt-"+string(rune('0'+attempt)), false)
		require.NoError(t, err)
	}
	current = base.Add(time.Duration(managerMFAMaxSends) * (managerMFASendCooldown + time.Second))
	_, err = service.claimSend(ctx, &challenge, "attempt-too-many")
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, "max_attempts", limited.Reason)
}

func TestManagerMFAAtomicVerifyAllowsOnlyOneConcurrentConsumer(t *testing.T) {
	service := newManagerMFAStateService(t)
	cfg := config.New()
	ctx := config.NewContext(cfg)
	email := "mfa-atomic-" + time.Now().Format("150405.000000000") + "@example.com"
	challengeID := "challenge-" + time.Now().Format("150405.000000000")
	code := "123456"
	attemptID := "attempt-atomic"
	keys := []string{
		commonbase.EmailCodeKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailCodeStatusKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailVerifyFailKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailVerifyLockKey(email, commonbase.CodeTypeManagerLogin),
		managerMFAActiveKey("atomic-uid"),
		managerMFASendStateKey(challengeID),
		managerMFAChallengeKey(challengeID),
	}
	t.Cleanup(func() {
		for _, key := range keys {
			_ = service.client.Del(key).Err()
		}
	})
	require.NoError(t, service.client.Set(keys[0], code, time.Minute).Err())
	require.NoError(t, service.client.Set(keys[1], "sent:"+attemptID, time.Minute).Err())
	require.NoError(t, service.client.HSet(keys[5], "status", "sent").Err())
	require.NoError(t, service.client.HSet(keys[5], "attempt_id", attemptID).Err())
	require.NoError(t, service.client.Expire(keys[5], time.Minute).Err())
	require.NoError(t, service.client.Set(keys[4], challengeID, time.Minute).Err())
	require.NoError(t, service.client.Set(keys[6], "challenge-payload", time.Minute).Err())

	emailService := commonbase.NewEmailService(ctx, nil)
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			results <- emailService.VerifyManagerCodeAtomically(
				context.Background(), email, code, challengeID,
				keys[4], keys[5], keys[6],
			)
		}()
	}
	successes := 0
	for i := 0; i < cap(results); i++ {
		if err := <-results; err == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes, "the Redis consume script must allow one verifier")
	assert.Empty(t, service.client.Get(keys[4]).Val(), "successful verification must consume the active challenge index")
	assert.Empty(t, service.client.Get(keys[6]).Val(), "successful verification must consume the challenge payload")
}

func TestManagerMFAAtomicVerifyReturnsVerificationLockRetryAfter(t *testing.T) {
	service := newManagerMFAStateService(t)
	cfg := config.New()
	ctx := config.NewContext(cfg)
	email := "mfa-lock-" + time.Now().Format("150405.000000000") + "@example.com"
	challengeID := "challenge-lock-" + time.Now().Format("150405.000000000")
	attemptID := "attempt-lock"
	keys := []string{
		commonbase.EmailCodeKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailCodeStatusKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailVerifyFailKey(email, commonbase.CodeTypeManagerLogin),
		commonbase.EmailVerifyLockKey(email, commonbase.CodeTypeManagerLogin),
		managerMFAActiveKey("lock-uid"),
		managerMFASendStateKey(challengeID),
		managerMFAChallengeKey(challengeID),
	}
	t.Cleanup(func() {
		for _, key := range keys {
			_ = service.client.Del(key).Err()
		}
	})
	require.NoError(t, service.client.Set(keys[0], "123456", time.Minute).Err())
	require.NoError(t, service.client.Set(keys[1], "sent:"+attemptID, time.Minute).Err())
	require.NoError(t, service.client.HSet(keys[5], "status", "sent").Err())
	require.NoError(t, service.client.HSet(keys[5], "attempt_id", attemptID).Err())
	require.NoError(t, service.client.Expire(keys[5], time.Minute).Err())
	require.NoError(t, service.client.Set(keys[4], challengeID, time.Minute).Err())
	require.NoError(t, service.client.Set(keys[6], "challenge-payload", time.Minute).Err())

	emailService := commonbase.NewEmailService(ctx, nil)
	for attempt := 1; attempt <= 2; attempt++ {
		err := emailService.VerifyManagerCodeAtomically(
			context.Background(), email, "000000", challengeID,
			keys[4], keys[5], keys[6],
		)
		assert.ErrorIs(t, err, commonbase.ErrManagerCodeInvalid)
	}

	var locked *commonbase.ManagerCodeLockedError
	err := emailService.VerifyManagerCodeAtomically(
		context.Background(), email, "000000", challengeID,
		keys[4], keys[5], keys[6],
	)
	require.ErrorAs(t, err, &locked)
	assert.GreaterOrEqual(t, locked.RetryAfter, 1)
	assert.LessOrEqual(t, locked.RetryAfter, 10*60)
	assert.ErrorIs(t, err, commonbase.ErrManagerCodeLocked)

	// A subsequent verification while the lock is already present must return
	// the remaining Redis TTL, not the generic ten-minute default. Shorten the
	// lock first so this assertion cannot pass if the implementation returns a
	// fixed 600-second value instead of driving the PTTL branch.
	require.NoError(t, service.client.Expire(keys[3], 5*time.Second).Err())
	var lockedAgain *commonbase.ManagerCodeLockedError
	err = emailService.VerifyManagerCodeAtomically(
		context.Background(), email, "000000", challengeID, keys[4], keys[5], keys[6],
	)
	require.ErrorAs(t, err, &lockedAgain)
	assert.GreaterOrEqual(t, lockedAgain.RetryAfter, 1)
	assert.LessOrEqual(t, lockedAgain.RetryAfter, 5)
}
