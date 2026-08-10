package user

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

func TestPasswordMutationAndSessionRevocationIntentAreDurableAndApplied(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	prefix := "user-revocation-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-revocation-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, auth.WithSessionMode(auth.SessionModeRevoke), auth.WithSessionMaxPerUID(4))
	uid := util.GenerUUID()
	token := "token-" + util.GenerUUID()
	password := "hash-before"
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "revocation-user", ShortNo: uid, Password: password, Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
		_ = store.DeleteToken(context.Background(), token)
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{UID: uid, DeviceFlag: int(config.Web)}, fence))
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.NoError(t, err)

	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "hash-after", "password_reset")
	require.NoError(t, err)
	require.NotZero(t, intent.ID)
	require.NotEmpty(t, intent.EventID)
	require.EqualValues(t, 1, intent.EventVersion)

	updated, err := db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, "hash-after", updated.Password)
	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending)

	require.NoError(t, applyAndMarkSessionRevocation(context.Background(), db, store, intent))
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.Error(t, err)
	var applied int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationApplied).Load(&applied)
	require.NoError(t, err)
	require.Equal(t, 1, applied)

	// An idempotent replay must not rotate away sessions created after the
	// original security event.
	postFence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	postToken := "post-" + util.GenerUUID()
	require.NoError(t, store.IssueNewSession(context.Background(), postToken, auth.TokenInfo{UID: uid, DeviceFlag: int(config.PC)}, postFence))
	t.Cleanup(func() { _ = store.DeleteToken(context.Background(), postToken) })
	require.NoError(t, applyAndMarkSessionRevocation(context.Background(), db, store, intent))
	_, err = auth.NewTokenValidator(store, prefix, auth.WithValidatorClock(func() time.Time { return time.Now() })).Validate(context.Background(), postToken)
	require.NoError(t, err)
}

func TestAccountDisableAndRevocationIntentCommitTogether(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	uid := util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "disable-user", ShortNo: uid, Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
	})

	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "status", "0", "account_disable")
	require.NoError(t, err)
	require.NotNil(t, intent)
	updated, err := db.QueryByUID(uid)
	require.NoError(t, err)
	require.Zero(t, updated.Status)

	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending)
}

func TestNewSessionRevocationIntentReservesSynchronousClaimWindow(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	uid := util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "sync-priority-user", ShortNo: uid, Password: "before", Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
	})

	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "after", "password_reset")
	require.NoError(t, err)
	now := time.Now().UTC()

	workerIntent, err := db.claimSessionRevocation(context.Background(), "worker", now)
	require.NoError(t, err)
	require.Nil(t, workerIntent, "a normal worker must not race the synchronous apply immediately after commit")

	claimed, err := db.claimSessionRevocationByID(context.Background(), intent, "sync", now)
	require.NoError(t, err)
	require.True(t, claimed, "the synchronous by-ID path must ignore its own worker visibility delay")
}

func TestSessionRevocationWorkerAppliesPendingIntent(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	db := NewDB(ctx)
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	prefix := "user-revocation-worker-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-revocation-worker-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, auth.WithSessionMode(auth.SessionModeRevoke), auth.WithSessionMaxPerUID(4))
	uid := util.GenerUUID()
	token := "token-" + util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "worker-user", ShortNo: uid, Password: "before", Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
		_ = store.DeleteToken(context.Background(), token)
		_ = client.Close()
	})

	fence, err := store.BeginIssue(context.Background(), uid)
	require.NoError(t, err)
	require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{UID: uid, DeviceFlag: int(config.Web)}, fence))
	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "after", "password_reset")
	require.NoError(t, err)
	_, err = ctx.DB().Update("user_session_revocation_intent").
		Set("next_attempt_at", time.Now().UTC().Add(-time.Second)).
		Where("id=?", intent.ID).
		Exec()
	require.NoError(t, err, "simulate a synchronous caller that exited before claiming its intent")

	worker := &User{db: db, sessionStore: store, revocationWorkerOwner: "test-worker"}
	worker.processPendingSessionRevocations()
	var applied int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").Where("id=? AND status=?", intent.ID, sessionRevocationApplied).Load(&applied)
	require.NoError(t, err)
	require.Equal(t, 1, applied)
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.Error(t, err)
}

func TestCommittedSessionRevocationIgnoresCanceledRequestContext(t *testing.T) {
	_, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	db := userAPI.db
	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	prefix := "user-revocation-canceled-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-revocation-canceled-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour, auth.WithSessionMode(auth.SessionModeRevoke), auth.WithSessionMaxPerUID(2))
	uid := "c-" + util.GenerUUID()
	token := "token-" + util.GenerUUID()
	require.NoError(t, db.Insert(&Model{UID: uid, Name: "canceled user", ShortNo: uid, Password: "before", Status: 1}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
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
	require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{
		UID: uid, DeviceFlag: int(config.Web), DeviceID: "web-device",
	}, fence))
	intent, err := db.updateUserFieldWithSessionRevocation(context.Background(), uid, "password", "after", "password_reset")
	require.NoError(t, err)

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, applyAndMarkSessionRevocation(requestCtx, db, store, intent),
		"a committed intent must use a bounded context detached from the canceled request")
	_, err = auth.NewTokenValidator(store, prefix).Validate(context.Background(), token)
	require.Error(t, err)
}

type logoutFailingSessionStore struct {
	fakeUserSessionStore
	err error
}

func TestFinishCommittedUserSecurityMutationPreservesCommitBoundary(t *testing.T) {
	t.Run("uncommitted mutation has no external side effects", func(t *testing.T) {
		store := &recordingDeviceRevokeStore{
			tokens: map[int]string{int(config.APP): "app-token"}, failures: make(map[string]error),
		}
		quitCalls := 0
		mutationErr := errors.New("database rollback")
		err := finishCommittedUserSecurityMutation(context.Background(), store, "u1", false, mutationErr, func(string, int) error {
			quitCalls++
			return nil
		})
		require.ErrorIs(t, err, mutationErr)
		require.Empty(t, store.revoked)
		require.Zero(t, quitCalls)
	})

	t.Run("committed compatibility mutation revokes bearers and always quits IM", func(t *testing.T) {
		store := &recordingDeviceRevokeStore{
			tokens: map[int]string{
				int(config.APP): "app-token", int(config.Web): "web-token", int(config.PC): "pc-token",
			},
			failures: map[string]error{"web-token": errors.New("redis unavailable")},
		}
		var quitFlags []int
		err := finishCommittedUserSecurityMutation(context.Background(), store, "u1", true, nil, func(_ string, flag int) error {
			quitFlags = append(quitFlags, flag)
			return nil
		})
		require.ErrorContains(t, err, "redis unavailable")
		require.ElementsMatch(t, []string{"app-token", "web-token", "pc-token"}, store.revoked)
		require.Equal(t, []int{-1}, quitFlags, "a bearer revocation error must not skip independent IM logout")
	})

	t.Run("committed compatibility mutation ignores request cancellation", func(t *testing.T) {
		store := &recordingDeviceRevokeStore{
			tokens: map[int]string{int(config.APP): "app-token"}, failures: make(map[string]error),
		}
		requestCtx, cancel := context.WithCancel(context.Background())
		cancel()
		quitCalls := 0
		err := finishCommittedUserSecurityMutation(requestCtx, store, "u1", true, nil, func(_ string, flag int) error {
			quitCalls++
			require.Equal(t, -1, flag)
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"app-token"}, store.revoked)
		require.Equal(t, 1, quitCalls)
	})

	t.Run("committed durable mutation still quits IM when synchronous apply fails", func(t *testing.T) {
		store := &flakyRevokeAllSessionStore{}
		applyErr := errors.New("redis unavailable")
		quitCalls := 0
		err := finishCommittedUserSecurityMutation(context.Background(), store, "u1", true, applyErr, func(_ string, flag int) error {
			quitCalls++
			require.Equal(t, -1, flag)
			return nil
		})
		require.ErrorIs(t, err, applyErr)
		require.Equal(t, 1, quitCalls)
	})
}

func TestInvalidateCurrentUserTokenIgnoresCanceledRequestContext(t *testing.T) {
	store := &recordingDeviceRevokeStore{
		tokens: map[int]string{int(config.APP): "app-token"}, failures: make(map[string]error),
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, invalidateCurrentUserToken(requestCtx, store, "u1", "app-token"))
	require.Equal(t, []string{"app-token"}, store.revoked)
}

func TestPasswordMutationPathsUseCommittedSecurityMutationHelper(t *testing.T) {
	legacyHashMigrations := map[string]bool{
		"api.go/login":                       true,
		"api_emaillogin.go/emailLogin":       true,
		"api_manager.go/login":               true,
		"api_usernamelogin.go/usernameLogin": true,
		"oidc_bind.go/VerifyPasswordByUID":   true,
	}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var mutationPaths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fset := gotoken.NewFileSet()
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		require.NoError(t, err)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || function.Name.Name == "updatePasswordAndRevokeSessions" {
				continue
			}
			mutatesPassword, usesHelper, legacyHashMigration := passwordMutationCalls(function.Body)
			if !mutatesPassword {
				continue
			}
			path := entry.Name() + "/" + function.Name.Name
			mutationPaths = append(mutationPaths, path)
			if legacyHashMigration && legacyHashMigrations[path] {
				continue
			}
			require.True(t, usesHelper, "%s writes the password credential without updatePasswordAndRevokeSessions", path)
		}
	}
	sort.Strings(mutationPaths)
	require.Contains(t, mutationPaths, "api_usernamelogin.go/resetPwdWithWeb3PublicKey")
	require.NotEmpty(t, mutationPaths)
}

func passwordMutationCalls(body *ast.BlockStmt) (mutatesPassword bool, usesHelper bool, legacyHashMigration bool) {
	var hasPasswordLiteral bool
	var callsGenericUserWriter bool
	var callsDirectPasswordWriter bool
	ast.Inspect(body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BasicLit:
			if typed.Kind == gotoken.STRING {
				value, err := strconv.Unquote(typed.Value)
				if err == nil && value == "password" {
					hasPasswordLiteral = true
				}
			}
		case *ast.CallExpr:
			name := calledFunctionName(typed.Fun)
			if name == "updatePasswordAndRevokeSessions" {
				usesHelper = true
			}
			if name == "updateUser" || name == "UpdateUsersWithField" {
				callsGenericUserWriter = true
			}
			if name == "updatePassword" {
				callsDirectPasswordWriter = true
			}
		}
		return true
	})
	mutatesPassword = usesHelper || callsDirectPasswordWriter || (hasPasswordLiteral && callsGenericUserWriter)
	legacyHashMigration = callsDirectPasswordWriter && !callsGenericUserWriter && !usesHelper
	return mutatesPassword, usesHelper, legacyHashMigration
}

func calledFunctionName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

func TestDisablePathUsesCommittedSecurityMutationHelper(t *testing.T) {
	fset := gotoken.NewFileSet()
	file, err := parser.ParseFile(fset, "api_manager.go", nil, 0)
	require.NoError(t, err)
	found := make(map[string]bool)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "liftBanUser" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok {
				found[calledFunctionName(call.Fun)] = true
			}
			return true
		})
	}
	require.True(t, found["updateUserFieldAndRevokeSessionsWithResult"])
	require.True(t, found["finishCommittedUserSecurityMutation"])
}

func TestPasswordMutationRevokesKnownBearersAndQuitsAllIMDevices(t *testing.T) {
	_, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "pwd-" + util.GenerUUID()
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Name: "password user", ShortNo: uid, Password: "before", Status: int(libcommon.UserAvailable),
	}))

	tokens := map[config.DeviceFlag]string{
		config.APP: "pwd-app-" + util.GenerUUID(),
		config.Web: "pwd-web-" + util.GenerUUID(),
		config.PC:  "pwd-pc-" + util.GenerUUID(),
	}
	for flag, token := range tokens {
		payload, err := auth.Encode(auth.TokenInfo{UID: uid, DeviceFlag: int(flag)})
		require.NoError(t, err)
		require.NoError(t, userAPI.sessionStore.IssueNew(context.Background(), token, payload, uid, int(flag)))
	}

	var quitFlagsMu sync.Mutex
	var quitFlags []int
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/user/device_quit", r.URL.Path)
		var request struct {
			DeviceFlag int `json:"device_flag"`
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, util.ReadJsonByByte(body, &request))
		quitFlagsMu.Lock()
		quitFlags = append(quitFlags, request.DeviceFlag)
		quitFlagsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	require.NoError(t, updatePasswordAndRevokeSessions(
		context.Background(), userAPI.db, userAPI.sessionStore, ctx.QuitUserDevice, uid, "after", "password_reset",
	))
	updated, err := userAPI.db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, "after", updated.Password)
	for flag, token := range tokens {
		indexed, err := userAPI.sessionStore.DeviceToken(context.Background(), uid, int(flag))
		require.NoError(t, err)
		require.Empty(t, indexed)
		record, err := auth.SessionStoreForContext(ctx).ReadToken(context.Background(), ctx.GetConfig().Cache.TokenCachePrefix+token)
		require.NoError(t, err)
		require.Equal(t, time.Duration(-2), record.TTL)
	}
	quitFlagsMu.Lock()
	require.Equal(t, []int{-1}, quitFlags)
	quitFlagsMu.Unlock()
}

func TestWeb3PasswordResetRevokesKnownBearersAndQuitsAllIMDevices(t *testing.T) {
	server, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "w3-" + util.GenerUUID()
	username := "web3_" + uid[len(uid)-12:]
	const (
		verifyText = "hello123"
		publicKey  = "03af80b90d25145da28c583359beb47b21796b2fe1a23c1511e443e7a64dfdb27d"
		signature  = "44459fd9146290dcd913350bae6fe79fd48050d39b3c1c315e8f032af3b555d41af6f2c07d4d0f1d8d5dd041af8175e657ae981cf47e58aa5547ab08fc7066e401"
	)
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Username: username, Name: "web3 user", ShortNo: username,
		Password: "before", Status: int(libcommon.UserAvailable), Web3PublicKey: publicKey,
	}))
	require.NoError(t, ctx.GetRedisConn().SetAndExpire("web3_verify:"+uid+"_"+Web3VerifyPassword, verifyText, time.Minute))

	tokens := map[config.DeviceFlag]string{
		config.APP: "web3-app-" + util.GenerUUID(),
		config.Web: "web3-web-" + util.GenerUUID(),
		config.PC:  "web3-pc-" + util.GenerUUID(),
	}
	for flag, token := range tokens {
		payload, err := auth.Encode(auth.TokenInfo{UID: uid, DeviceFlag: int(flag)})
		require.NoError(t, err)
		require.NoError(t, userAPI.sessionStore.IssueNew(context.Background(), token, payload, uid, int(flag)))
	}

	var quitFlagsMu sync.Mutex
	var quitFlags []int
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/user/device_quit", r.URL.Path)
		var request struct {
			DeviceFlag int `json:"device_flag"`
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, util.ReadJsonByByte(body, &request))
		quitFlagsMu.Lock()
		quitFlags = append(quitFlags, request.DeviceFlag)
		quitFlagsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL
	server.GetRoute().POST("/v1/user/pwdforget_web3", userAPI.resetPwdWithWeb3PublicKey)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/user/pwdforget_web3", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": username, "password": "after", "verify_text": verifyText, "sign_text": signature,
	}))))
	request.Header.Set("Content-Type", "application/json")
	server.GetRoute().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	updated, err := userAPI.db.QueryByUID(uid)
	require.NoError(t, err)
	matched, _ := CheckPassword("after", updated.Password)
	require.True(t, matched)
	for _, token := range tokens {
		record, err := auth.SessionStoreForContext(ctx).ReadToken(context.Background(), ctx.GetConfig().Cache.TokenCachePrefix+token)
		require.NoError(t, err)
		require.Equal(t, time.Duration(-2), record.TTL)
	}
	quitFlagsMu.Lock()
	require.Equal(t, []int{-1}, quitFlags)
	quitFlagsMu.Unlock()
}

func TestV3WriteCompatibilityRevocationFreesBoundedIndex(t *testing.T) {
	cfg := config.New()
	client := octoredis.NewInstrumentedClient(cfg)
	prefix := "user-v3-index-token:" + util.GenerUUID() + ":"
	uidPrefix := "user-v3-index-uid:" + util.GenerUUID() + ":"
	store := auth.NewRedisSessionStore(client, prefix, uidPrefix, time.Hour,
		auth.WithSessionMode(auth.SessionModeV3Write), auth.WithSessionMaxPerUID(3))
	uid := "v3-index-" + util.GenerUUID()
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
	for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
		flagName := strconv.Itoa(int(flag))
		token := "before-" + flagName + "-" + util.GenerUUID()
		require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{
			UID: uid, DeviceFlag: int(flag), DeviceID: "device-" + flagName,
		}, fence))
	}
	require.Empty(t, revokeDeviceTokens(context.Background(), store, uid))

	for _, flag := range []config.DeviceFlag{config.APP, config.Web, config.PC} {
		flagName := strconv.Itoa(int(flag))
		token := "after-" + flagName + "-" + util.GenerUUID()
		require.NoError(t, store.IssueNewSession(context.Background(), token, auth.TokenInfo{
			UID: uid, DeviceFlag: int(flag), DeviceID: "replacement-" + flagName,
		}, fence), "all bounded-index slots must be reusable after compatibility revocation")
	}
}

func TestDisableUserStillAppliesIMSideEffectsAfterCommittedRevocationFailure(t *testing.T) {
	server, ctx, _, managerAPI := newTokenHTTPTestServer(t)
	wireI18nRendererForUserTest(server)
	uid := "disable-" + util.GenerUUID()
	require.NoError(t, managerAPI.userDB.Insert(&Model{
		UID: uid, Name: "disable user", ShortNo: uid, Status: int(libcommon.UserAvailable),
	}))

	var imMu sync.Mutex
	deviceQuitCalls := 0
	channelCalls := 0
	var sideEffectOrder []string
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imMu.Lock()
		switch r.URL.Path {
		case "/user/device_quit":
			deviceQuitCalls++
			sideEffectOrder = append(sideEffectOrder, "quit")
		default:
			channelCalls++
			sideEffectOrder = append(sideEffectOrder, "channel")
		}
		imMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":0}`))
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	store := &flakyRevokeAllSessionStore{}
	managerAPI.sessionStore = store
	server.GetRoute().Any("/test/manager/liftban/:uid/:status", func(c *wkhttp.Context) {
		c.Set("uid", "root")
		c.Set("role", string(wkhttp.SuperAdmin))
		managerAPI.liftBanUser(c)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/test/manager/liftban/"+uid+"/0", nil)
	server.GetRoute().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	updated, err := managerAPI.userDB.QueryByUID(uid)
	require.NoError(t, err)
	require.Zero(t, updated.Status, "the account disable transaction must remain committed")
	require.Equal(t, 1, store.callCount())
	imMu.Lock()
	require.Equal(t, 1, deviceQuitCalls, "post-commit Redis failure must not skip independent IM logout")
	require.Equal(t, 1, channelCalls, "post-commit Redis failure must not skip the IM channel ban")
	require.Equal(t, []string{"channel", "quit"}, sideEffectOrder, "the channel ban must be visible before disconnecting devices")
	imMu.Unlock()

	retryRecorder := httptest.NewRecorder()
	retryRequest := httptest.NewRequest(http.MethodPut, "/test/manager/liftban/"+uid+"/0", nil)
	server.GetRoute().ServeHTTP(retryRecorder, retryRequest)
	require.Equal(t, http.StatusOK, retryRecorder.Code, retryRecorder.Body.String())
	require.Equal(t, 2, store.callCount(), "an already-disabled retry must re-apply the pending revocation intent")
	imMu.Lock()
	require.Equal(t, 2, deviceQuitCalls)
	require.Equal(t, 2, channelCalls)
	require.Equal(t, []string{"channel", "quit", "channel", "quit"}, sideEffectOrder)
	imMu.Unlock()
}

func (s *logoutFailingSessionStore) InvalidateCurrentToken(context.Context, string, string) error {
	return s.err
}

func TestLogoutStillQuitsIMAndCleansDeviceStateWhenBearerRevokeFails(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(server)
	uid := "logout-converge-" + util.GenerUUID()
	deviceKey := "logout-device:" + uid
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(deviceKey, "present", time.Minute))

	var flagsMu sync.Mutex
	var flags []int
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/device_quit" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":0}`))
			return
		}
		var request struct {
			DeviceFlag int `json:"device_flag"`
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, util.ReadJsonByByte(body, &request))
		flagsMu.Lock()
		flags = append(flags, request.DeviceFlag)
		flagsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	u := New(ctx)
	u.sessionStore = &logoutFailingSessionStore{err: errors.New("redis unavailable")}
	u.userDeviceTokenPrefix = "logout-device:"
	server.GetRoute().POST("/test/session/logout", func(c *wkhttp.Context) {
		c.Set("uid", uid)
		u.quit(c)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/test/session/logout", nil)
	request.Header.Set("token", "current-token")
	server.GetRoute().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	flagsMu.Lock()
	require.ElementsMatch(t, []int{int(config.Web), int(config.PC)}, flags,
		"HTTP bearer failure must not skip independent IM logout attempts")
	flagsMu.Unlock()
	deviceState, err := ctx.GetRedisConn().GetString(deviceKey)
	require.NoError(t, err)
	require.Empty(t, deviceState, "a retryable bearer failure must not skip local device-state cleanup")
}

type flakyRevokeAllSessionStore struct {
	fakeUserSessionStore
	mu    sync.Mutex
	calls int
}

func (s *flakyRevokeAllSessionStore) Mode() auth.SessionMode { return auth.SessionModeRevoke }

func (s *flakyRevokeAllSessionStore) BeginIssue(context.Context, string) (auth.IssueFence, error) {
	return auth.IssueFence{}, nil
}

func (s *flakyRevokeAllSessionStore) IssueNewSession(context.Context, string, auth.TokenInfo, auth.IssueFence) error {
	return nil
}

func (s *flakyRevokeAllSessionStore) ReuseSession(context.Context, string, auth.TokenInfo, auth.IssueFence) (bool, error) {
	return false, nil
}

func (s *flakyRevokeAllSessionStore) UpdateSessionSnapshot(context.Context, string, auth.TokenInfo) (bool, error) {
	return false, nil
}

func (s *flakyRevokeAllSessionStore) RevokeCurrent(context.Context, string, string, int) error {
	return nil
}

func (s *flakyRevokeAllSessionStore) RevokeAll(context.Context, string, auth.RevocationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls == 1 {
		return errors.New("redis unavailable")
	}
	return nil
}

func (s *flakyRevokeAllSessionStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type allowDestroySMSService struct{}

func (allowDestroySMSService) SendVerifyCode(context.Context, string, string, commonapi.CodeType) error {
	return nil
}

func (allowDestroySMSService) Verify(context.Context, string, string, string, commonapi.CodeType) error {
	return nil
}

func TestDestroyAccountRetryConvergesCommittedRevocationAndAlwaysQuitsIM(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(server)
	require.NoError(t, testutil.CleanAllTables(ctx))
	ctx.GetConfig().SMSCode = "123456"
	uid := "d-" + util.GenerUUID()
	db := NewDB(ctx)
	require.NoError(t, db.Insert(&Model{
		UID:       uid,
		Name:      "destroy converge user",
		Username:  "destroy_" + uid[len(uid)-12:],
		ShortNo:   "destroy_" + uid[len(uid)-10:],
		Phone:     "13800000000",
		Zone:      "0086",
		Status:    int(libcommon.UserAvailable),
		IsDestroy: IsDestroyNo,
	}))
	t.Cleanup(func() {
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_intent").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user_session_revocation_version").Where("uid=?", uid).Exec()
		_, _ = ctx.DB().DeleteFrom("user").Where("uid=?", uid).Exec()
	})

	var imMu sync.Mutex
	imCalls := 0
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/device_quit" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":0}`))
			return
		}
		imMu.Lock()
		imCalls++
		imMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	store := &flakyRevokeAllSessionStore{}
	u := New(ctx)
	u.sessionStore = store
	u.smsServie = allowDestroySMSService{}
	server.GetRoute().Any("/test/session/destroy/:code", func(c *wkhttp.Context) {
		c.Set("uid", uid)
		u.destroyAccount(c)
	})

	requestDestroy := func() *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodDelete, "/test/session/destroy/123456", nil)
		server.GetRoute().ServeHTTP(recorder, request)
		return recorder
	}

	first := requestDestroy()
	require.Equal(t, http.StatusBadRequest, first.Code, first.Body.String())
	model, err := db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, IsDestroyDone, model.IsDestroy, "the first response failed after the DB transaction committed")
	imMu.Lock()
	require.Equal(t, 1, imCalls, "post-commit Redis failure must not skip the independent IM quit")
	imMu.Unlock()

	second := requestDestroy()
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.Equal(t, 2, store.callCount(), "retry must apply the existing pending revocation intent")
	imMu.Lock()
	require.Equal(t, 2, imCalls, "retry must keep IM quit idempotent")
	imMu.Unlock()
	var applied int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").
		Where("uid=? AND status=?", uid, sessionRevocationApplied).Load(&applied)
	require.NoError(t, err)
	require.Equal(t, 1, applied)
}
