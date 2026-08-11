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
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

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

func TestTokenValidatorV3SteadyStateUsesExactlyTwoReadsAndNoWrites(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-command-count:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-command-count-uid:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		metadata, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, TokenInfo{UID: uid, DeviceFlag: 1}, fence))
	require.NoError(t, readTokenScript.Load(client).Err(), "preload the read script so NOSCRIPT fallback is outside the steady-state count")

	var commands []string
	client.WrapProcess(func(old func(rd.Cmder) error) func(rd.Cmder) error {
		return func(cmd rd.Cmder) error {
			commands = append(commands, cmd.Name())
			return old(cmd)
		}
	})

	_, err = NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, []string{"evalsha", "get"}, commands,
		"steady-state v3 validation must remain token single-key Lua plus generation GET, with no Redis write")
}

func TestRedisSessionStoreRevokeIssuedRemovesV3IndexReservation(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-compensation:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-compensation-uid:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		metadata, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		token := "failed-" + util.GenerUUID()
		require.NoError(t, store.IssueNewSession(context.Background(), token, TokenInfo{
			UID:        uid,
			DeviceFlag: i,
			DeviceID:   "failed-device-" + string(rune('a'+i)),
		}, fence))
		require.NoError(t, store.RevokeIssued(context.Background(), token, uid, i))
	}

	count, err := client.ZCard(store.sessionIndexKey(uid, fence.Generation)).Result()
	require.NoError(t, err)
	require.Zero(t, count, "downstream login compensation must not consume bounded-session capacity")
	require.NoError(t, store.IssueNewSession(context.Background(), "recovered-"+util.GenerUUID(), TokenInfo{
		UID:        uid,
		DeviceFlag: 1,
		DeviceID:   "recovered-device",
	}, fence), "a recovered dependency must be able to issue immediately after compensated failures")
}

func TestRedisSessionStoreRevokeIssuedPreservesV3SessionAdoptedByAnotherReplica(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-v3-adopted:" + util.GenerUUID() + ":"
	uidPrefix := "session-v3-adopted-uid:" + util.GenerUUID() + ":"
	storeA := NewRedisSessionStore(clientA, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	storeB := NewRedisSessionStore(clientB, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := clientA.Keys(prefix + "*").Result()
		metadata, _ := clientA.Keys(uidPrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = clientA.Del(keys...).Err()
		}
		_ = clientA.Close()
		_ = clientB.Close()
	})

	fence, err := storeA.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	info := TokenInfo{UID: uid, Name: "first", DeviceFlag: 1, DeviceID: "shared-device"}
	require.NoError(t, storeA.IssueNewSession(context.Background(), token, info, fence))
	adopted, err := storeB.ReuseSession(context.Background(), token, TokenInfo{
		UID:        uid,
		Name:       "adopted",
		DeviceFlag: 1,
		DeviceID:   "shared-device",
	}, fence)
	require.NoError(t, err)
	require.True(t, adopted)

	require.NoError(t, storeA.RevokeIssued(context.Background(), token, uid, 1))
	validated, err := NewTokenValidator(storeB, prefix).Validate(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, "adopted", validated.Name)
	count, err := clientA.ZCard(storeA.sessionIndexKey(uid, fence.Generation)).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "an old issuer must not erase the adopted replica's bounded-index membership")
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

func TestInvalidateCurrentLegacyTokenCompareDeletesCompatibilityIndex(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-legacy-logout:" + util.GenerUUID() + ":"
	uidPrefix := "session-legacy-logout-uid:" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour)
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		metadata, _ := client.Keys(uidPrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
	})

	payload, err := Encode(TokenInfo{UID: uid, Name: "legacy"})
	require.NoError(t, err)
	require.NoError(t, store.IssueNew(context.Background(), token, payload, uid, 1))
	require.NoError(t, store.InvalidateCurrentToken(context.Background(), uid, token))

	indexed, err := store.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Empty(t, indexed, "legacy logout must not leave uidtoken pointing at a revoked bearer")
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

func TestPromoteLegacySessionConflictCleansOwnIndexReservation(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-promote-conflict:" + util.GenerUUID() + ":"
	uidPrefix := "session-promote-conflict-uid:" + util.GenerUUID() + ":"
	now := time.Date(2026, time.August, 10, 10, 0, 0, 0, time.UTC)
	store := NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, WithSessionMode(SessionModeV3Write), WithSessionMaxPerUID(2), WithSessionClock(func() time.Time { return now }))
	uid := "u-" + util.GenerUUID()
	token := "t-" + util.GenerUUID()
	legacy, err := Encode(TokenInfo{UID: uid, Name: "old"})
	require.NoError(t, err)
	require.NoError(t, client.Set(prefix+token, legacy, 20*time.Minute).Err())
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
	staleRecord, err := store.ReadToken(context.Background(), prefix+token)
	require.NoError(t, err)

	ok, conflict, err := store.promoteLegacySession(context.Background(), token, staleRecord, TokenInfo{
		UID: uid, Name: "winner", DeviceFlag: 1, DeviceID: "winner-device",
	}, fence)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, conflict)

	ok, conflict, err = store.promoteLegacySession(context.Background(), token, staleRecord, TokenInfo{
		UID: uid, Name: "loser", DeviceFlag: 1, DeviceID: "loser-device",
	}, fence)
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, conflict)

	members, err := client.ZRange(store.sessionIndexKey(uid, fence.Generation), 0, -1).Result()
	require.NoError(t, err)
	winnerMember, err := encodeSessionIndexMember(token, 1, "winner-device")
	require.NoError(t, err)
	require.Equal(t, []string{winnerMember}, members, "losing promotion must not consume a bounded-session slot")
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

// Configuration is advisory now. The deprecated variables are honoured for one
// release but a stale or malformed one must never take the process down: a
// deployment failing to start over an environment variable is the failure class
// this redesign removes.
func TestSessionPolicyFromEnvIsAdvisoryAndNeverFatal(t *testing.T) {
	t.Run("no env at all", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		policy := sessionPolicyFromEnv()
		require.Empty(t, policy.legacyMode)
		require.Zero(t, policy.legacyMaxPerUID)
		require.Empty(t, policy.warnings)
	})

	t.Run("deprecated mode is carried and flagged", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		t.Setenv(sessionModeEnv, string(SessionModeBounded))
		t.Setenv(sessionMaxPerUIDEnv, "20")
		policy := sessionPolicyFromEnv()
		require.Equal(t, SessionModeBounded, policy.legacyMode)
		require.Equal(t, 20, policy.legacyMaxPerUID)
		require.Len(t, policy.warnings, 2)
	})

	for _, value := range []string{"", "unknown", "V3-WRITE"} {
		t.Run("unparseable mode "+value+" is ignored, not fatal", func(t *testing.T) {
			unsetSessionRuntimeEnv(t)
			t.Setenv(sessionModeEnv, value)
			policy := sessionPolicyFromEnv()
			require.Empty(t, policy.legacyMode)
			require.NotEmpty(t, policy.warnings)
		})
	}

	t.Run("removed required-floor is reported, not honoured", func(t *testing.T) {
		unsetSessionRuntimeEnv(t)
		t.Setenv(sessionRequiredFloorEnv, string(SessionModeV3Write))
		policy := sessionPolicyFromEnv()
		require.NotEmpty(t, policy.warnings)
	})
}

// The cap moves from the environment into the floor record. A #725 record
// predates the field, so reading one must work and the next advance must be
// able to supply it.
func TestSessionRolloutControlCarriesMaxPerUIDAndReadsLegacyRecords(t *testing.T) {
	store, client := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()

	require.NoError(t, client.Set(store.rolloutControlKey(),
		`{"mode_floor":"v3-write","writer_version":3,"observation_min_gap_ms":3600000}`, 0).Err())
	control, err := store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeV3Write, control.ModeFloor)
	require.Zero(t, control.MaxPerUID, "a #725 record has no cap field")

	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeRevoke, 20))
	control, err = store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, SessionModeRevoke, control.ModeFloor)
	require.Equal(t, 20, control.MaxPerUID)
	require.Equal(t, int64(3600000), control.ObservationMinGapMS,
		"the unused gap is carried forward so a #725 artifact can still parse the record")

	// Omitting the cap on a later advance keeps the persisted one.
	require.NoError(t, store.AdvanceRolloutControl(ctx, SessionModeBounded, 0))
	control, err = store.RolloutControl(ctx)
	require.NoError(t, err)
	require.Equal(t, 20, control.MaxPerUID)
}

// A v3-writing floor without a cap would be a bounded index with no bound.
func TestSessionRolloutControlRefusesV3FloorWithoutCap(t *testing.T) {
	store, _ := newLegacyMigrationTestStore(t, SessionModeRevoke)
	ctx := context.Background()
	require.ErrorContains(t, store.AdvanceRolloutControl(ctx, SessionModeV3Write, 0),
		"bounded max sessions per UID")
}
