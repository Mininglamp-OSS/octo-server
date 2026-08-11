package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

func TestRedisSessionStoreDeviceLookupAndCompensation(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	uid := "session-compensation-" + util.GenerUUID()
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1" + uid
	token := "session-issued-" + util.GenerUUID()
	replacement := "session-replacement-" + util.GenerUUID()
	t.Cleanup(func() {
		_ = client.Del(cfg.Cache.TokenCachePrefix+token, uidKey).Err()
		_ = client.Close()
	})

	payload, err := Encode(TokenInfo{UID: uid, Name: "issued"})
	require.NoError(t, err)
	require.NoError(t, store.IssueNew(context.Background(), token, payload, uid, 1))
	got, err := store.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Equal(t, token, got)

	// A newer replica may have replaced the compatibility index before an
	// older issue attempt compensates. RevokeIssued must never erase the newer
	// index; compare-delete limits cleanup to the credential it owns.
	require.NoError(t, client.Set(uidKey, replacement, time.Minute).Err())
	require.NoError(t, store.RevokeIssued(context.Background(), token, uid, 1))
	got, err = store.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Equal(t, replacement, got)
	exists, err := client.Exists(cfg.Cache.TokenCachePrefix + token).Result()
	require.NoError(t, err)
	require.Zero(t, exists)

	require.NoError(t, client.Del(uidKey).Err())
	got, err = store.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestRedisSessionStoreCompensationPreservesTokenAdoptedByAnotherReplica(t *testing.T) {
	cfg := config.New()
	clientA := octoredis.NewInstrumentedClient(cfg)
	clientB := octoredis.NewInstrumentedClient(cfg)
	storeA := NewRedisSessionStore(clientA, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	storeB := NewRedisSessionStore(clientB, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	uid := "session-adopted-" + util.GenerUUID()
	token := "session-adopted-token-" + util.GenerUUID()
	tokenKey := cfg.Cache.TokenCachePrefix + token
	uidKey := cfg.Cache.UIDTokenCachePrefix + "1" + uid
	t.Cleanup(func() {
		_ = clientA.Del(tokenKey, uidKey).Err()
		_ = clientA.Close()
		_ = clientB.Close()
	})
	payload, err := Encode(TokenInfo{UID: uid, Name: "same-payload"})
	require.NoError(t, err)

	// Replica A publishes a new credential, then stalls while updating IM.
	require.NoError(t, storeA.IssueNew(context.Background(), token, payload, uid, 1))

	// Replica B observes the compatibility index, adopts the same token, and
	// completes its login. The payload is deliberately byte-identical so the
	// store cannot mistake payload equality for issue-attempt ownership.
	indexedToken, err := storeB.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Equal(t, token, indexedToken)
	adopted, err := storeB.ReuseExisting(context.Background(), token, payload, uid, 1)
	require.NoError(t, err)
	require.True(t, adopted)

	// Replica A's IM call now fails. Its compensation must not revoke the
	// credential that replica B has already adopted and returned to a client.
	require.NoError(t, storeA.RevokeIssued(context.Background(), token, uid, 1))
	record, err := storeB.ReadToken(context.Background(), tokenKey)
	require.NoError(t, err)
	require.Equal(t, payload, record.Payload)
	require.Positive(t, record.TTL)
	indexedToken, err = storeB.DeviceToken(context.Background(), uid, 1)
	require.NoError(t, err)
	require.Equal(t, token, indexedToken)
}

func TestRedisSessionStoreReadsAtomicPayloadAndTTLStates(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "session-read-" + util.GenerUUID() + ":"
	store := NewRedisSessionStore(client, prefix, "session-read-uid:", time.Minute)
	finiteKey := prefix + "finite"
	persistentKey := prefix + "persistent"
	t.Cleanup(func() {
		_ = client.Del(finiteKey, persistentKey).Err()
		_ = client.Close()
	})

	require.NoError(t, client.Set(finiteKey, "finite-payload", time.Minute).Err())
	require.NoError(t, client.Set(persistentKey, "persistent-payload", 0).Err())

	finite, err := store.ReadToken(context.Background(), finiteKey)
	require.NoError(t, err)
	require.Equal(t, "finite-payload", finite.Payload)
	require.Positive(t, finite.TTL)

	persistent, err := store.ReadToken(context.Background(), persistentKey)
	require.NoError(t, err)
	require.Equal(t, "persistent-payload", persistent.Payload)
	require.Equal(t, time.Duration(-1), persistent.TTL)

	missing, err := store.ReadToken(context.Background(), prefix+"missing")
	require.NoError(t, err)
	require.Equal(t, time.Duration(-2), missing.TTL)
}

func TestRedisSessionStoreRejectsInvalidInputsAndCancelledWork(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	store := NewRedisSessionStore(client, cfg.Cache.TokenCachePrefix, cfg.Cache.UIDTokenCachePrefix, time.Minute)
	t.Cleanup(func() { _ = client.Close() })

	require.Error(t, store.IssueNew(context.Background(), "", "payload", "u1", 1))
	_, err := store.ReuseExisting(context.Background(), "", "payload", "u1", 1)
	require.Error(t, err)
	_, err = store.UpdatePayloadKeepDeadline(context.Background(), "", "payload")
	require.Error(t, err)
	_, err = store.DeviceToken(context.Background(), "", 1)
	require.Error(t, err)
	require.Error(t, store.DeleteToken(context.Background(), ""))
	require.Error(t, store.RevokeIssued(context.Background(), "token", "", 1))
	_, err = store.ReadToken(context.Background(), "")
	require.Error(t, err)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, store.IssueNew(cancelled, "token", "payload", "u1", 1), context.Canceled)
	_, err = store.ReuseExisting(cancelled, "token", "payload", "u1", 1)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.DeviceToken(cancelled, "u1", 1)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.DeleteToken(cancelled, "token"), context.Canceled)
	require.ErrorIs(t, store.RevokeIssued(cancelled, "token", "u1", 1), context.Canceled)
	_, err = store.ReadToken(cancelled, "token:key")
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.ObserveRateLimited(cancelled, 1, 0)
	require.ErrorIs(t, err, context.Canceled)

	_, err = store.ObserveRateLimited(context.Background(), 0, 0)
	require.Error(t, err)
	_, err = store.ObserveRateLimited(context.Background(), 1, -time.Millisecond)
	require.Error(t, err)
}

func TestSessionStoreConstructorsRejectUnsafeConfiguration(t *testing.T) {
	require.Panics(t, func() { NewRedisSessionStore(nil, "token:", "uidtoken:", time.Minute) })

	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	t.Cleanup(func() { _ = client.Close() })
	require.Panics(t, func() { NewRedisSessionStore(client, "token:", "uidtoken:", 0) })
	require.Panics(t, func() { SessionStoreForContext(nil) })

	ctx := config.NewContext(cfg)
	store, runtimeClient := SessionStoreAndClientForContext(ctx)
	t.Cleanup(func() { _ = runtimeClient.Close() })
	require.Same(t, store, SessionStoreForContext(ctx))
}

func TestRedisIntegerAcceptsRedisRepresentations(t *testing.T) {
	for _, tt := range []struct {
		name    string
		value   interface{}
		want    int64
		wantErr bool
	}{
		{name: "integer", value: int64(7), want: 7},
		{name: "string", value: "-2", want: -2},
		{name: "bytes", value: []byte("42"), want: 42},
		{name: "malformed", value: "nope", wantErr: true},
		{name: "unexpected type", value: 7, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := redisInteger(tt.value)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestTokenValidatorFailsClosedAcrossStoreAndGenerationErrors(t *testing.T) {
	now := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	v3, err := EncodeV3(TokenInfo{
		UID:               "u1",
		IssuedAt:          now.Add(-time.Minute).Unix(),
		ExpiresAt:         now.Add(time.Minute).Unix(),
		SessionGeneration: "g1",
		SessionRevision:   1,
	})
	require.NoError(t, err)
	v2, err := Encode(TokenInfo{UID: "u1"})
	require.NoError(t, err)
	storeErr := errors.New("redis unavailable")
	generationErr := errors.New("generation unavailable")

	tests := []struct {
		name       string
		token      string
		reader     *fakeTokenRecordReader
		resolver   SessionGenerationResolver
		want       error
		wrappedErr error
	}{
		{name: "missing token input", token: " ", reader: &fakeTokenRecordReader{}, want: wkhttp.ErrTokenMissing},
		{name: "store failure", token: "tok", reader: &fakeTokenRecordReader{err: storeErr}, wrappedErr: storeErr},
		{name: "empty payload", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {TTL: time.Minute}}}, want: wkhttp.ErrTokenNotFound},
		{name: "decode failure", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: "invalid", TTL: time.Minute}}}, want: wkhttp.ErrTokenInvalid},
		{name: "v3 zero ttl", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v3, TTL: 0}}}, want: wkhttp.ErrTokenInvalid},
		{name: "v3 resolver missing", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v3, TTL: time.Minute}}}, want: wkhttp.ErrTokenInvalid},
		{name: "v3 resolver failure", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v3, TTL: time.Minute}}}, resolver: fakeSessionGenerationResolver{err: generationErr}, wrappedErr: generationErr},
		{name: "v3 empty generation", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v3, TTL: time.Minute}}}, resolver: fakeSessionGenerationResolver{}, want: wkhttp.ErrTokenInvalid},
		{name: "legacy zero ttl", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v2, TTL: 0}}}, want: wkhttp.ErrTokenInvalid},
		{name: "legacy invalid negative ttl", token: "tok", reader: &fakeTokenRecordReader{records: map[string]TokenRecord{testPrefix + "tok": {Payload: v2, TTL: -3}}}, want: wkhttp.ErrTokenInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewTokenValidator(
				tt.reader,
				testPrefix,
				WithValidatorClock(func() time.Time { return now }),
				WithSessionGenerationResolver(tt.resolver),
			)
			_, err := validator.Validate(context.Background(), tt.token)
			if tt.wrappedErr != nil {
				require.ErrorIs(t, err, tt.wrappedErr)
				return
			}
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestTokenValidatorConstructorAndNilOptions(t *testing.T) {
	require.Panics(t, func() { NewTokenValidator(nil, testPrefix) })
	reader := &fakeTokenRecordReader{records: map[string]TokenRecord{}}
	validator := NewTokenValidator(
		reader,
		testPrefix,
		WithValidatorClock(nil),
		WithSessionGenerationResolver(nil),
	)
	require.NotNil(t, validator.now)
}
