package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

func TestSessionPolicyFromEnvDefaultsClosedAndValidatesActivation(t *testing.T) {
	t.Run("expand default", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		policy, err := sessionPolicyFromEnv()
		require.NoError(t, err)
		require.Equal(t, SessionModeExpand, policy.mode)
		require.Zero(t, policy.maxPerUID)
	})

	t.Run("v3 requires explicit bound", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		t.Setenv(sessionModeEnv, string(SessionModeV3Write))
		_, err := sessionPolicyFromEnv()
		require.ErrorContains(t, err, sessionMaxPerUIDEnv)
	})

	t.Run("v3 accepts explicit bound", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		t.Setenv(sessionModeEnv, string(SessionModeV3Write))
		t.Setenv(sessionMaxPerUIDEnv, "2")
		policy, err := sessionPolicyFromEnv()
		require.NoError(t, err)
		require.Equal(t, SessionModeV3Write, policy.mode)
		require.Equal(t, 2, policy.maxPerUID)
	})

	t.Run("required floor rejects lower configured mode", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		t.Setenv(sessionRequiredFloorEnv, string(SessionModeV3Write))
		_, err := sessionPolicyFromEnv()
		require.ErrorContains(t, err, sessionRequiredFloorEnv)
	})

	for _, value := range []string{"", "unknown", "V3-WRITE"} {
		t.Run("invalid mode "+value, func(t *testing.T) {
			unsetSessionRuntimeEnv(t)
			t.Setenv(sessionModeEnv, value)
			_, err := sessionPolicyFromEnv()
			require.Error(t, err)
		})
	}
}

func TestSessionRolloutControlIsMonotonicAndFailClosed(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-rollout:" + util.GenerUUID() + ":"
	uidPrefix := "session-rollout-uid:" + util.GenerUUID() + ":"
	v3Store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	expandStore := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour)
	t.Cleanup(func() {
		_ = client.Del(v3Store.rolloutControlKey()).Err()
		_ = client.Close()
	})

	require.NoError(t, v3Store.ValidateRolloutControl(context.Background(), ""))
	require.Error(t, v3Store.ValidateRolloutControl(context.Background(), SessionModeV3Write))
	require.NoError(t, v3Store.AdvanceRolloutControl(context.Background(), SessionModeV3Write))
	require.Error(t, expandStore.ValidateRolloutControl(context.Background(), ""))
	require.NoError(t, v3Store.ValidateRolloutControl(context.Background(), SessionModeV3Write))
	require.Error(t, v3Store.AdvanceRolloutControl(context.Background(), SessionModeBounded))
	require.NoError(t, v3Store.AdvanceRolloutControl(context.Background(), SessionModeRevoke))
	control, err := v3Store.RolloutControl(context.Background())
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, control.ModeFloor)
}

func TestRedisSessionStoreIssuesBoundedV3AndValidatorUsesGeneration(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-uid:" + util.GenerUUID() + ":"
	now := time.Date(2026, time.August, 9, 13, 0, 0, 0, time.UTC)
	store := NewRedisSessionStore(
		client,
		prefix,
		uidPrefix,
		time.Hour,
		WithSessionMode(SessionModeV3Write),
		WithSessionMaxPerUID(2),
		WithSessionClock(func() time.Time { return now }),
	)
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NotEmpty(t, fence.Generation)
	require.Positive(t, fence.Revision)

	err = store.IssueNewSession(context.Background(), token, TokenInfo{
		UID:        uid,
		Name:       "alice",
		DeviceFlag: 1,
		DeviceID:   "device-1",
	}, fence)
	require.NoError(t, err)

	record, err := store.ReadToken(context.Background(), prefix+token)
	require.NoError(t, err)
	require.Positive(t, record.TTL)
	require.LessOrEqual(t, record.TTL, time.Hour)
	info, err := Decode(record.Payload)
	require.NoError(t, err)
	require.True(t, info.IsV3())
	require.Equal(t, fence.Generation, info.SessionGeneration)
	require.Equal(t, fence.Revision, info.SessionRevision)
	require.Equal(t, now.Unix(), info.IssuedAt)
	require.Equal(t, now.Add(time.Hour).Unix(), info.ExpiresAt)

	validator := NewTokenValidator(store, prefix, WithValidatorClock(func() time.Time { return now }))
	got, err := validator.Validate(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, uid, got.UID)
}

func TestRedisSessionStoreV3IndexIsBounded(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-cap:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-cap-uid:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(
		client,
		prefix,
		uidPrefix,
		time.Hour,
		WithSessionMode(SessionModeV3Write),
		WithSessionMaxPerUID(2),
	)
	uid := "u-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		err = store.IssueNewSession(context.Background(), "token-"+util.GenerUUID(), TokenInfo{
			UID:        uid,
			DeviceFlag: i,
		}, fence)
		require.NoError(t, err)
	}
	err = store.IssueNewSession(context.Background(), "token-"+util.GenerUUID(), TokenInfo{
		UID:        uid,
		DeviceFlag: 2,
	}, fence)
	require.ErrorIs(t, err, ErrSessionLimitReached)
}

func TestRedisSessionStoreGenerationDeadlineNeverShrinks(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-generation:" + util.GenerUUID() + ":"
	uidPrefix := "session-generation-uid:" + util.GenerUUID() + ":"
	longStore := NewRedisSessionStore(clientA, prefix, uidPrefix, 2*time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	shortStore := NewRedisSessionStore(clientB, prefix, uidPrefix, time.Minute, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := clientA.Keys(uidPrefix + "*").Result()
		if len(keys) > 0 {
			_ = clientA.Del(keys...).Err()
		}
		_ = clientA.Close()
		_ = clientB.Close()
	})

	first, err := longStore.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	key := longStore.generationKey(uid)
	before, err := clientA.PTTL(key).Result()
	require.NoError(t, err)
	require.Greater(t, before, time.Hour)

	second, err := shortStore.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, first, second)
	after, err := clientA.PTTL(key).Result()
	require.NoError(t, err)
	require.Greater(t, after, time.Hour)
	require.LessOrEqual(t, after, before)
}

func TestRedisSessionStoreRejectsStaleIssueFence(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-fence:" + util.GenerUUID() + ":"
	uidPrefix := "session-fence-uid:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.RevokeAll(context.Background(), uid, RevocationEvent{Version: 1, ID: "password-reset"}))
	err = store.IssueNewSession(context.Background(), token, TokenInfo{UID: uid, DeviceFlag: 1}, fence)
	require.ErrorIs(t, err, ErrIssueFenceChanged)
	exists, existsErr := client.Exists(prefix + token).Result()
	require.NoError(t, existsErr)
	require.Zero(t, exists)

	err = store.RevokeAll(context.Background(), uid, RevocationEvent{Version: 1, ID: "password-reset"})
	require.True(t, err == nil || errors.Is(err, ErrRevocationAlreadyApplied))
}

func TestRedisSessionStoreConcurrentIndexNeverExceedsBound(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-concurrent:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-concurrent-uid:" + util.GenerUUID() + ":"
	storeA := NewRedisSessionStore(clientA, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	storeB := NewRedisSessionStore(clientB, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := clientA.Keys(prefix + "*").Result()
		meta, _ := clientA.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = clientA.Del(keys...).Err()
		}
		_ = clientA.Close()
		_ = clientB.Close()
	})

	fence, err := storeA.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	otherFence, err := storeB.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.Equal(t, fence, otherFence)

	var successes atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := storeA
			if i%2 == 1 {
				store = storeB
			}
			err := store.IssueNewSession(context.Background(), "token-"+util.GenerUUID(), TokenInfo{UID: uid, DeviceFlag: i}, fence)
			if err == nil {
				successes.Add(1)
				return
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	require.EqualValues(t, 2, successes.Load())
	for err := range errs {
		require.ErrorIs(t, err, ErrSessionLimitReached)
	}
	count, err := clientA.ZCard(storeA.sessionIndexKey(uid, fence.Generation)).Result()
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
}

func TestRedisSessionStoreRevocationRetryDoesNotRevokePostEventSession(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-retry:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-retry-uid:" + util.GenerUUID() + ":"
	storeA := NewRedisSessionStore(clientA, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeRevoke), WithSessionMaxPerUID(2))
	storeB := NewRedisSessionStore(clientB, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeRevoke), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	tokenA := "t-" + util.GenerUUID()
	tokenB := "t-" + util.GenerUUID()
	event := RevocationEvent{Version: 1, ID: "event-" + util.GenerUUID()}
	t.Cleanup(func() {
		keys, _ := clientA.Keys(prefix + "*").Result()
		meta, _ := clientA.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = clientA.Del(keys...).Err()
		}
		_ = clientA.Close()
		_ = clientB.Close()
	})

	_, err := storeA.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, storeA.RevokeAll(context.Background(), uid, event))
	postEventFence, err := storeB.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, storeB.IssueNewSession(context.Background(), tokenA, TokenInfo{UID: uid, DeviceFlag: 1}, postEventFence))
	require.NoError(t, storeB.IssueNewSession(context.Background(), tokenB, TokenInfo{UID: uid, DeviceFlag: 2}, postEventFence))
	require.ErrorIs(t, storeA.RevokeAll(context.Background(), uid, event), ErrRevocationAlreadyApplied)

	validator := NewTokenValidator(storeA, prefix)
	_, err = validator.Validate(context.Background(), tokenA)
	require.NoError(t, err)
	_, err = validator.Validate(context.Background(), tokenB)
	require.NoError(t, err)
	err = storeA.IssueNewSession(context.Background(), "t-"+util.GenerUUID(), TokenInfo{UID: uid, DeviceFlag: 3}, postEventFence)
	require.ErrorIs(t, err, ErrSessionLimitReached,
		"replaying an old revocation event must not erase post-event sessions from the bounded index")
}

func TestTokenValidatorAppliesLegacyRolloutPolicy(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-legacy-policy:" + util.GenerUUID() + ":"
	uidPrefix := "session-legacy-policy-uid:" + util.GenerUUID() + ":"
	uid := "u-" + util.GenerUUID()
	payload, err := Encode(TokenInfo{UID: uid, Name: "legacy"})
	require.NoError(t, err)
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	t.Run("v3 writer honors an existing deny marker", func(t *testing.T) {
		store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
		token := "denied-" + util.GenerUUID()
		require.NoError(t, client.Set(prefix+token, payload, time.Minute).Err())
		require.NoError(t, client.Set(store.legacyDenyKey(uid), "1", 0).Err())
		_, err := NewTokenValidator(store, prefix).Validate(context.Background(), token)
		require.ErrorIs(t, err, wkhttp.ErrTokenInvalid)
		require.NoError(t, client.Del(store.legacyDenyKey(uid)).Err())
	})

	t.Run("bounded rejects persistent legacy", func(t *testing.T) {
		store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeBounded), WithSessionMaxPerUID(2))
		token := "persistent-" + util.GenerUUID()
		require.NoError(t, client.Set(prefix+token, payload, 0).Err())
		_, err := NewTokenValidator(store, prefix).Validate(context.Background(), token)
		require.ErrorIs(t, err, wkhttp.ErrTokenInvalid)
	})

	t.Run("enforce rejects finite legacy without a marker lookup", func(t *testing.T) {
		store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeEnforce), WithSessionMaxPerUID(2))
		token := "enforce-" + util.GenerUUID()
		require.NoError(t, client.Set(prefix+token, payload, time.Minute).Err())
		_, err := NewTokenValidator(store, prefix).Validate(context.Background(), token)
		require.ErrorIs(t, err, wkhttp.ErrTokenInvalid)
	})
}

func TestRedisSessionStorePromotesLegacyReuseWithoutExtendingDeadline(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-promote:" + util.GenerUUID() + ":"
	uidPrefix := "session-promote-uid:" + util.GenerUUID() + ":"
	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2), WithSessionClock(func() time.Time { return now }))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	legacy, err := Encode(TokenInfo{UID: uid, Name: "old"})
	require.NoError(t, err)
	require.NoError(t, client.Set(prefix+token, legacy, 20*time.Minute).Err())
	before, err := client.PTTL(prefix + token).Result()
	require.NoError(t, err)
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	ok, err := store.ReuseSession(context.Background(), token, TokenInfo{UID: uid, Name: "new", DeviceFlag: 1, DeviceID: "device-1"}, fence)
	require.NoError(t, err)
	require.True(t, ok)
	after, err := client.PTTL(prefix + token).Result()
	require.NoError(t, err)
	require.Positive(t, after)
	require.LessOrEqual(t, after, before)
	record, err := store.ReadToken(context.Background(), prefix+token)
	require.NoError(t, err)
	info, err := Decode(record.Payload)
	require.NoError(t, err)
	require.True(t, info.IsV3())
	require.Equal(t, "new", info.Name)
	require.Equal(t, fence.Generation, info.SessionGeneration)
	require.Equal(t, fence.Revision, info.SessionRevision)
	require.Equal(t, now.Unix(), info.IssuedAt)
	require.LessOrEqual(t, info.ExpiresAt, now.Add(20*time.Minute).Unix())
}

func TestRedisSessionStoreSnapshotUpdatePreservesV3SecurityClaims(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-snapshot:" + util.GenerUUID() + ":"
	uidPrefix := "session-snapshot-uid:" + util.GenerUUID() + ":"
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2), WithSessionClock(func() time.Time { return now }))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		meta, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, meta...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, TokenInfo{UID: uid, Name: "old", Role: "user", Language: "en-US", DeviceFlag: 1, DeviceID: "device-1"}, fence))
	beforeRecord, err := store.ReadToken(context.Background(), prefix+token)
	require.NoError(t, err)
	beforeInfo, err := Decode(beforeRecord.Payload)
	require.NoError(t, err)

	ok, err := store.UpdateSessionSnapshot(context.Background(), token, TokenInfo{UID: uid, Name: "new", Role: "admin", Language: "zh-CN"})
	require.NoError(t, err)
	require.True(t, ok)
	afterRecord, err := store.ReadToken(context.Background(), prefix+token)
	require.NoError(t, err)
	afterInfo, err := Decode(afterRecord.Payload)
	require.NoError(t, err)
	require.Equal(t, "new", afterInfo.Name)
	require.Equal(t, "admin", afterInfo.Role)
	require.Equal(t, "zh-CN", afterInfo.Language)
	require.Equal(t, beforeInfo.IssuedAt, afterInfo.IssuedAt)
	require.Equal(t, beforeInfo.ExpiresAt, afterInfo.ExpiresAt)
	require.Equal(t, beforeInfo.DeviceFlag, afterInfo.DeviceFlag)
	require.Equal(t, beforeInfo.DeviceID, afterInfo.DeviceID)
	require.Equal(t, beforeInfo.SessionGeneration, afterInfo.SessionGeneration)
	require.Equal(t, beforeInfo.SessionRevision, afterInfo.SessionRevision)
	require.LessOrEqual(t, afterRecord.TTL, beforeRecord.TTL)
}
