package user

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

func TestSessionRolloutConcurrentFirstTakeoverConvergesWithoutStartupFailure(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	control := auth.NewRolloutControlStore(ctx.DB())
	for round := 0; round < 10; round++ {
		require.NoError(t, deleteRolloutControlRows(ctx))

		const starters = 8
		start := make(chan struct{})
		errs := make(chan error, starters)
		var wg sync.WaitGroup
		for i := 0; i < starters; i++ {
			seed := auth.RolloutSeed{
				Floor:     auth.SessionModeRevoke,
				MaxPerUID: 20,
				Actor:     "integration-startup",
				Source:    "legacy-redis",
			}
			if i%2 == 0 {
				seed.Floor = auth.SessionModeBounded
				seed.MaxPerUID = 12
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := control.Initialize(context.Background(), seed)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "every replica must survive concurrent first takeover")
		}

		state, err := control.Load(context.Background())
		require.NoError(t, err)
		require.Equal(t, auth.SessionModeBounded, state.Floor)
		require.Equal(t, 12, state.MaxPerUID)
	}
}

func TestSessionRolloutMySQLControlLifecycleAndSingleWinner(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, deleteRolloutControlRows(ctx))

	var tables []string
	_, err := ctx.DB().SelectBySql(`
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = DATABASE()
		  AND table_name IN ('octo_session_rollout_state', 'octo_session_rollout_advance')
		ORDER BY table_name
	`).Load(&tables)
	require.NoError(t, err)
	require.Equal(t, []string{"octo_session_rollout_advance", "octo_session_rollout_state"}, tables,
		"the real user-module migration must create both control tables")

	control := auth.NewRolloutControlStore(ctx.DB())
	state, err := control.Initialize(context.Background(), auth.RolloutSeed{
		Floor: auth.SessionModeExpand, MaxPerUID: 20, Actor: "integration-startup", Source: "bootstrap",
	})
	require.NoError(t, err)
	require.Equal(t, auth.SessionModeExpand, state.Floor)
	require.EqualValues(t, 1, state.Version)

	require.NoError(t, control.SetPaused(context.Background(), true))
	paused, err := control.Load(context.Background())
	require.NoError(t, err)
	require.True(t, paused.Paused)

	_, err = control.Advance(context.Background(), paused, auth.RolloutAdvanceRecord{
		From: paused.Floor, To: auth.SessionModeV3Write, Actor: "paused-integration",
	})
	require.ErrorIs(t, err, auth.ErrRolloutControlChanged)
	require.EqualValues(t, 1, countRolloutAuditRows(t, ctx),
		"a paused CAS loser must roll its evidence back")

	require.NoError(t, control.SetPaused(context.Background(), false))
	current, err := control.Load(context.Background())
	require.NoError(t, err)
	require.False(t, current.Paused)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, advanceErr := control.Advance(context.Background(), current, auth.RolloutAdvanceRecord{
				From:              current.Floor,
				To:                auth.SessionModeV3Write,
				Actor:             "racing-integration",
				RedisID:           "redis-run-id",
				WriterFingerprint: "writer-fingerprint",
			})
			errs <- advanceErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	winners := 0
	losers := 0
	for advanceErr := range errs {
		switch {
		case advanceErr == nil:
			winners++
		case errors.Is(advanceErr, auth.ErrRolloutControlChanged):
			losers++
		default:
			require.NoError(t, advanceErr)
		}
	}
	require.Equal(t, 1, winners)
	require.Equal(t, 1, losers)
	require.EqualValues(t, 2, countRolloutAuditRows(t, ctx),
		"bootstrap plus exactly one committed advance must remain")

	advanced, err := control.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, auth.SessionModeV3Write, advanced.Floor)
	require.EqualValues(t, current.Version+1, advanced.Version)
}

func TestSessionRolloutAdoptedV3LoginAndAuthenticatedReadE2E(t *testing.T) {
	t.Setenv("OCTO_AUTH_SESSION_MODE", "")
	t.Setenv("OCTO_AUTH_SESSION_MAX_PER_UID", "")
	t.Setenv("OCTO_AUTH_SESSION_AUTO_ADVANCE", "0")

	s, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(s)
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, deleteRolloutControlRows(ctx))

	store := auth.SessionStoreForContext(ctx)
	client := store.Client()
	uidPrefix := store.UIDTokenPrefix()
	s.GetRoute().SetTokenParser(auth.NewCacheTokenParser(
		ctx.Cache(),
		ctx.GetConfig().Cache.TokenCachePrefix,
		auth.WithTokenValidator(auth.NewTokenValidator(store, ctx.GetConfig().Cache.TokenCachePrefix)),
		auth.WithLanguageResolver(NewLanguageService(NewDB(ctx), ctx.Cache())),
		auth.WithRoleResolver(NewRoleService(NewDB(ctx), ctx.Cache())),
	))
	require.NoError(t, clearRolloutRegistryKeys(client, uidPrefix))
	t.Cleanup(func() { _ = clearRolloutRegistryKeys(client, uidPrefix) })

	legacyControl, err := json.Marshal(auth.SessionRolloutControl{
		ModeFloor:           auth.SessionModeV3Write,
		WriterVersion:       3,
		ObservationMinGapMS: 0,
		MaxPerUID:           2,
	})
	require.NoError(t, err)
	require.NoError(t, client.Set(uidPrefix+"auth:rollout-control", legacyControl, 0).Err())

	boot, _, err := auth.InitializeSessionRollout(ctx)
	require.NoError(t, err)
	require.Equal(t, auth.RolloutBootAdopted, boot.Outcome)
	require.Equal(t, auth.SessionModeV3Write, boot.Floor)
	require.Equal(t, 2, boot.MaxPerUID)

	rolloutCtx, stopRollout := context.WithCancel(context.Background())
	t.Cleanup(stopRollout)
	registry := auth.NewWriterRegistry(client, uidPrefix)
	store.UseWriterLease(registry)
	require.NoError(t, registry.Join(rolloutCtx, "integration-build", "integration-pod", string(store.Mode()), nil))
	require.NoError(t, store.ApplyAndPublishRolloutState(registry, auth.RolloutState{
		Floor: boot.Floor, MaxPerUID: boot.MaxPerUID, Version: boot.Version,
	}, store.Mode()))
	require.NoError(t, store.CanIssue())

	password := "Pwd@12345"
	passwordHash, err := HashPassword(password)
	require.NoError(t, err)
	u := New(ctx)
	require.Same(t, store, u.sessionStore)
	const (
		uid      = "session-rollout-e2e-user"
		username = "rolloute2e01"
	)
	require.NoError(t, clearRolloutUserKeys(client, uidPrefix, uid))
	t.Cleanup(func() { _ = clearRolloutUserKeys(client, uidPrefix, uid) })

	// octo-lib's module registry caches the first test Context for the process.
	// Register dedicated routes backed by this test's User so the HTTP flow cannot
	// accidentally exercise a store or writer lease owned by an earlier test.
	s.GetRoute().POST("/_test/session-rollout/login", u.usernameLogin)
	currentGroup := s.GetRoute().Group("/_test/session-rollout", ctx.AuthMiddleware(s.GetRoute()))
	currentGroup.GET("/current", u.currentUser)
	require.NoError(t, u.db.Insert(&Model{
		UID: uid, Username: username, Name: "Rollout E2E", Password: passwordHash,
		ShortNo: "rollout01", Status: 1,
	}))
	require.True(t, registry.MayWrite())
	require.NoError(t, store.CanIssue())

	loginBody := util.ToJson(map[string]interface{}{
		"username": username,
		"password": password,
		"flag":     int(config.APP),
		"device": map[string]interface{}{
			"device_id": "rollout-e2e-device", "device_name": "e2e", "device_model": "integration",
		},
	})
	loginReq := httptest.NewRequest(http.MethodPost, "/_test/session-rollout/login", bytes.NewBufferString(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	setPublicIPForUserTest(loginReq, "198.51.100.73")
	loginResp := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(loginResp, loginReq)
	require.Equal(t, http.StatusOK, loginResp.Code, "body=%s", loginResp.Body.String())

	var loginEnvelope struct {
		Data loginUserDetailResp `json:"data"`
	}
	require.NoError(t, json.Unmarshal(loginResp.Body.Bytes(), &loginEnvelope))
	login := loginEnvelope.Data
	require.Equal(t, uid, login.UID, "body=%s", loginResp.Body.String())
	require.NotEmpty(t, login.Token)
	t.Cleanup(func() {
		_ = store.RevokeIssued(context.Background(), login.Token, uid, int(config.APP))
	})

	record, err := store.ReadToken(context.Background(), ctx.GetConfig().Cache.TokenCachePrefix+login.Token)
	require.NoError(t, err)
	tokenInfo, err := auth.Decode(record.Payload)
	require.NoError(t, err)
	require.True(t, tokenInfo.IsV3(), "the adopted v3-write floor must issue a v3 session")
	require.Equal(t, uid, tokenInfo.UID)

	currentReq := httptest.NewRequest(http.MethodGet, "/_test/session-rollout/current", nil)
	currentReq.Header.Set("token", login.Token)
	setPublicIPForUserTest(currentReq, "198.51.100.73")
	currentResp := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(currentResp, currentReq)
	require.Equal(t, http.StatusOK, currentResp.Code, "body=%s", currentResp.Body.String())
	var current loginUserDetailResp
	require.NoError(t, json.Unmarshal(currentResp.Body.Bytes(), &current))
	require.Equal(t, uid, current.UID, "body=%s", currentResp.Body.String())
	require.Equal(t, login.Token, current.Token)
}

func deleteRolloutControlRows(ctx *config.Context) error {
	if _, err := ctx.DB().Exec("DELETE FROM octo_session_rollout_advance"); err != nil {
		return err
	}
	_, err := ctx.DB().Exec("DELETE FROM octo_session_rollout_state")
	return err
}

func countRolloutAuditRows(t *testing.T, ctx *config.Context) int64 {
	t.Helper()
	var count int64
	err := ctx.DB().QueryRow("SELECT COUNT(*) FROM octo_session_rollout_advance").Scan(&count)
	require.NoError(t, err)
	return count
}

type rolloutRedisClient interface {
	Keys(string) *rd.StringSliceCmd
	Del(...string) *rd.IntCmd
}

func clearRolloutRegistryKeys(client rolloutRedisClient, uidPrefix string) error {
	keys, err := client.Keys(uidPrefix + "writer:*").Result()
	if err != nil {
		return err
	}
	keys = append(keys,
		uidPrefix+"writers",
		uidPrefix+"auth:rollout-control",
		uidPrefix+"auth:rollout-scan-owner",
	)
	return client.Del(keys...).Err()
}

func clearRolloutUserKeys(client rolloutRedisClient, uidPrefix, uid string) error {
	subject := sha256.Sum256([]byte(uid))
	encodedSubject := hex.EncodeToString(subject[:])
	keys, err := client.Keys(uidPrefix + "auth:sessions:" + encodedSubject + ":*").Result()
	if err != nil {
		return err
	}
	keys = append(keys,
		uidPrefix+"auth:generation:"+encodedSubject,
		uidPrefix+"auth:legacy-deny:"+encodedSubject,
		uidPrefix+"0"+uid,
	)
	return client.Del(keys...).Err()
}
