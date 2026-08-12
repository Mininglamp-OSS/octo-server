package user

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/module"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	libserver "github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/require"
)

var tokenHTTPTestDatabases struct {
	sync.Mutex
	names []string
}

func TestTokenDeadlineOverHTTP(t *testing.T) {
	server, ctx, _, _ := newTokenHTTPTestServer(t)
	t.Run("manager login issues a finite token", func(t *testing.T) {
		testManagerLoginIssuesFiniteTokenOverHTTP(t, server, ctx)
	})
	t.Run("nickname update preserves the deadline", func(t *testing.T) {
		testNicknameUpdatePreservesFiniteTokenDeadlineOverHTTP(t, server, ctx)
	})
}

func TestLoginFailureAuditUsesProxyAwareIPOverHTTP(t *testing.T) {
	server, ctx, _, _ := newTokenHTTPTestServer(t)
	username := "missing-login-" + util.GenerUUID()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": username,
		"password": "wrong-password",
		"flag":     int(config.Web),
	}))))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-For", "203.0.113.10, 198.51.100.24")
	server.GetRoute().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())

	var rows []*LoginLogModel
	_, err := ctx.DB().Select("*").From("login_log").
		Where("login_type=?", "username").OrderAsc("id").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, loginStatusFailure, rows[0].Status)
	require.Equal(t, "198.51.100.24", rows[0].LoginIP)
}

func TestLocalCredentialCannotCrossPasswordChangeBeforeIssueFence(t *testing.T) {
	t.Setenv("OCTO_AUTH_SESSION_MODE", string(auth.SessionModeV3Write))
	t.Setenv("OCTO_AUTH_SESSION_MAX_PER_UID", "10")
	server, ctx, userAPI, managerAPI := newTokenHTTPTestServer(t)
	realStore, ok := userAPI.sessionStore.(*auth.RedisSessionStore)
	require.True(t, ok)

	t.Run("user login rechecks password after fence", func(t *testing.T) {
		uid := util.GenerUUID()
		username := "fenceduser" + uid[:8]
		oldPassword := "old-user-password"
		newPassword := "new-user-password"
		oldHash, err := HashPassword(oldPassword)
		require.NoError(t, err)
		newHash, err := HashPassword(newPassword)
		require.NoError(t, err)
		require.NoError(t, userAPI.db.Insert(&Model{
			UID:      uid,
			Username: username,
			Name:     "Fenced User",
			ShortNo:  "usr_" + uid[:12],
			Password: oldHash,
			Status:   1,
		}))
		loaded, err := userAPI.db.QueryByUsername(username)
		require.NoError(t, err)
		require.NotNil(t, loaded)
		matched, _ := CheckPassword(oldPassword, loaded.Password)
		require.True(t, matched)
		require.Equal(t, auth.SessionModeV3Write, realStore.Mode())
		mutationCalled := false
		userAPI.sessionStore = &passwordChangeOnBeginIssueStore{
			RedisSessionStore: realStore,
			mutate: func() error {
				mutationCalled = true
				return userAPI.db.UpdateUsersWithField("password", newHash, uid)
			},
		}

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/user/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"username": username,
			"password": oldPassword,
			"flag":     int(config.Web),
		}))))
		request.Header.Set("Content-Type", "application/json")
		server.GetRoute().ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
		fresh, err := userAPI.db.QueryByUID(uid)
		require.NoError(t, err)
		require.True(t, mutationCalled, "test must mutate the password inside BeginIssue; body=%s", recorder.Body.String())
		matched, _ = CheckPassword(newPassword, fresh.Password)
		require.True(t, matched, "the committed password change must remain authoritative")
	})

	t.Run("manager login rechecks password after fence", func(t *testing.T) {
		uid := util.GenerUUID()
		username := "fenced-manager-" + uid[:12]
		oldPassword := "old-manager-password"
		newPassword := "new-manager-password"
		oldHash, err := HashPassword(oldPassword)
		require.NoError(t, err)
		newHash, err := HashPassword(newPassword)
		require.NoError(t, err)
		require.NoError(t, managerAPI.userDB.Insert(&Model{
			UID:      uid,
			Username: username,
			Name:     "Fenced Manager",
			ShortNo:  "mgr_" + uid[:12],
			Password: oldHash,
			Role:     string(wkhttp.SuperAdmin),
			Status:   1,
		}))
		managerAPI.sessionStore = &passwordChangeOnBeginIssueStore{
			RedisSessionStore: realStore,
			mutate: func() error {
				return managerAPI.userDB.UpdateUsersWithField("password", newHash, uid)
			},
		}

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"username": username,
			"password": oldPassword,
		}))))
		request.Header.Set("Content-Type", "application/json")
		server.GetRoute().ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
		fresh, err := managerAPI.userDB.QueryByUID(uid)
		require.NoError(t, err)
		require.Equal(t, newHash, fresh.Password)
	})

	t.Run("manager login rechecks account state after fence", func(t *testing.T) {
		uid := util.GenerUUID()
		username := "disabled-manager-" + uid[:12]
		password := "manager-password"
		hash, err := HashPassword(password)
		require.NoError(t, err)
		require.NoError(t, managerAPI.userDB.Insert(&Model{
			UID:      uid,
			Username: username,
			Name:     "Disabled Manager",
			ShortNo:  "mgr_" + uid[:12],
			Password: hash,
			Role:     string(wkhttp.SuperAdmin),
			Status:   int(common.UserAvailable),
		}))
		managerAPI.sessionStore = &passwordChangeOnBeginIssueStore{
			RedisSessionStore: realStore,
			mutate: func() error {
				_, err := managerAPI.userDB.session.Update("user").
					Set("status", int(common.UserDisable)).
					Where("uid=?", uid).
					Exec()
				return err
			},
		}

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"username": username,
			"password": password,
		}))))
		request.Header.Set("Content-Type", "application/json")
		server.GetRoute().ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code, "body=%s", recorder.Body.String())
		fresh, err := managerAPI.userDB.QueryByUID(uid)
		require.NoError(t, err)
		require.Equal(t, int(common.UserDisable), fresh.Status)
	})

	t.Run("external login rechecks account state after fence", func(t *testing.T) {
		uid := util.GenerUUID()
		require.NoError(t, userAPI.db.Insert(&Model{
			UID:      uid,
			Username: "external" + uid[:8],
			Name:     "Fenced External User",
			ShortNo:  "ext_" + uid[:12],
			Status:   1,
		}))
		mutationCalled := false
		userAPI.sessionStore = &passwordChangeOnBeginIssueStore{
			RedisSessionStore: realStore,
			mutate: func() error {
				mutationCalled = true
				_, err := userAPI.db.session.Update("user").Set("status", int(common.UserDisable)).Where("uid=?", uid).Exec()
				return err
			},
		}

		result, err := userAPI.externalLoginExisting(context.Background(), ExternalLoginReq{
			ExistingUID: uid,
			DeviceFlag:  config.Web,
		})
		require.Error(t, err)
		require.True(t, mutationCalled, "test must mutate status inside BeginIssue; err=%v", err)
		require.Nil(t, result)
		fresh, queryErr := userAPI.db.QueryByUID(uid)
		require.NoError(t, queryErr)
		require.Equal(t, int(common.UserDisable), fresh.Status)
	})

	_, client := auth.SessionStoreAndClientForContext(ctx)
	keys, err := client.Keys(ctx.GetConfig().Cache.TokenCachePrefix + "*").Result()
	require.NoError(t, err)
	require.Empty(t, keys, "stale credentials must not produce bearer keys")
}

type passwordChangeOnBeginIssueStore struct {
	*auth.RedisSessionStore
	once   sync.Once
	mutate func() error
	err    error
}

func (s *passwordChangeOnBeginIssueStore) BeginIssue(ctx context.Context, uid string) (auth.IssueFence, error) {
	s.once.Do(func() {
		if s.mutate != nil {
			s.err = s.mutate()
		}
	})
	if s.err != nil {
		return auth.IssueFence{}, s.err
	}
	return s.RedisSessionStore.BeginIssue(ctx, uid)
}

func testManagerLoginIssuesFiniteTokenOverHTTP(t *testing.T, server *libserver.Server, ctx *config.Context) {
	t.Helper()
	manager := NewManager(ctx)
	uid := util.GenerUUID()
	username := "manager-ttl-" + uid[:12]
	password := "manager-ttl-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	require.NoError(t, manager.userDB.Insert(&Model{
		UID:      uid,
		Username: username,
		Name:     "TTL Manager",
		ShortNo:  "mgr_" + uid[:12],
		Password: hash,
		Role:     string(wkhttp.SuperAdmin),
		Status:   1,
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/manager/login", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"username": username,
		"password": password,
	}))))
	request.Header.Set("Content-Type", "application/json")
	server.GetRoute().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())
	var response struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotEmpty(t, response.Token)

	_, client := auth.SessionStoreAndClientForContext(ctx)
	tokenKey := ctx.GetConfig().Cache.TokenCachePrefix + response.Token
	uidKey := ctx.GetConfig().Cache.UIDTokenCachePrefix + "1" + uid
	t.Cleanup(func() { _ = client.Del(tokenKey, uidKey).Err() })
	ttl, err := client.PTTL(tokenKey).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, ctx.GetConfig().Cache.TokenExpire)
}

func testNicknameUpdatePreservesFiniteTokenDeadlineOverHTTP(t *testing.T, server *libserver.Server, ctx *config.Context) {
	t.Helper()
	user := New(ctx)
	uid := util.GenerUUID()
	require.NoError(t, user.db.Insert(&Model{
		UID:      uid,
		Username: uid,
		Name:     "Before",
		ShortNo:  "usr_" + uid[:12],
		Status:   1,
	}))

	store, client := auth.SessionStoreAndClientForContext(ctx)
	tokenValue := "nickname-token-" + util.GenerUUID()
	require.NoError(t, store.IssueNew(context.Background(), tokenValue, uid+"@Before", uid, int(config.APP)))
	tokenKey := ctx.GetConfig().Cache.TokenCachePrefix + tokenValue
	uidKey := ctx.GetConfig().Cache.UIDTokenCachePrefix + "0" + uid
	t.Cleanup(func() { _ = client.Del(tokenKey, uidKey).Err() })
	before, err := client.PTTL(tokenKey).Result()
	require.NoError(t, err)
	require.Positive(t, before)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/user/current", bytes.NewReader([]byte(`{"name":"After"}`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("token", tokenValue)
	server.GetRoute().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, "body=%s", recorder.Body.String())

	after, err := client.PTTL(tokenKey).Result()
	require.NoError(t, err)
	require.Positive(t, after)
	require.LessOrEqual(t, after, before, "profile updates must not extend the bearer deadline")
}

func newTokenHTTPTestServer(t *testing.T) (*libserver.Server, *config.Context, *User, *Manager) {
	t.Helper()
	databaseName := "octo_user_token_ttl_" + util.GenerUUID()[:12]
	bootstrapConfig := config.New()
	bootstrapConfig.Test = true
	bootstrapConfig.DB.MySQLAddr = "root:demo@tcp(127.0.0.1:3306)/information_schema?charset=utf8mb4&parseTime=true"
	bootstrap := config.NewContext(bootstrapConfig)
	_, err := bootstrap.DB().Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err, "create isolated token TTL test database")
	require.NoError(t, bootstrap.DB().DB.Close())
	tokenHTTPTestDatabases.Lock()
	tokenHTTPTestDatabases.names = append(tokenHTTPTestDatabases.names, databaseName)
	tokenHTTPTestDatabases.Unlock()

	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = fmt.Sprintf("root:demo@tcp(127.0.0.1:3306)/%s?charset=utf8mb4&parseTime=true", databaseName)
	cfg.Cache.TokenCachePrefix = "token-http:" + util.GenerUUID() + ":"
	cfg.Cache.UIDTokenCachePrefix = "token-http-uid:" + util.GenerUUID() + ":"
	ctx := config.NewContext(cfg)

	// module.Setup must run the complete migration set, but octo-lib caches its
	// module instances behind a process-wide sync.Once. Under CI's shuffled
	// package tests those instances may belong to an earlier `test` database
	// context, so routes registered by Setup must not serve this isolated DB.
	migrationServer := libserver.New(ctx)
	migrationServer.GetRoute().UseGin(ctx.Tracer().GinMiddle())
	ctx.SetHttpRoute(migrationServer.GetRoute())
	require.NoError(t, module.Setup(ctx), "set up modules in isolated token TTL database")

	// Register only the handlers exercised here from objects constructed with
	// this test's context. This keeps handler reads and fixture writes on the
	// same isolated database regardless of which package test initialized the
	// global module registry first.
	server := libserver.New(ctx)
	server.GetRoute().UseGin(ctx.Tracer().GinMiddle())
	ctx.SetHttpRoute(server.GetRoute())
	store, client := auth.SessionStoreAndClientForContext(ctx)
	server.GetRoute().SetTokenParser(auth.NewCacheTokenParser(
		ctx.Cache(),
		cfg.Cache.TokenCachePrefix,
		auth.WithTokenValidator(auth.NewTokenValidator(store, cfg.Cache.TokenCachePrefix)),
	))
	userAPI := New(ctx)
	managerAPI := NewManager(ctx)
	server.GetRoute().POST("/v1/manager/login", managerAPI.login)
	server.GetRoute().POST("/v1/user/login", userAPI.login)
	server.GetRoute().Group("/v1/user", ctx.AuthMiddleware(server.GetRoute())).PUT("/current", userAPI.userUpdateWithField)
	t.Cleanup(func() {
		keys, _ := client.Keys(cfg.Cache.TokenCachePrefix + "*").Result()
		metadata, _ := client.Keys(cfg.Cache.UIDTokenCachePrefix + "*").Result()
		keys = append(keys, metadata...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
	})
	return server, ctx, userAPI, managerAPI
}

func cleanupTokenHTTPTestDatabases() error {
	tokenHTTPTestDatabases.Lock()
	names := append([]string(nil), tokenHTTPTestDatabases.names...)
	tokenHTTPTestDatabases.names = nil
	tokenHTTPTestDatabases.Unlock()
	if len(names) == 0 {
		return nil
	}

	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = "root:demo@tcp(127.0.0.1:3306)/information_schema?charset=utf8mb4&parseTime=true"
	bootstrap := config.NewContext(cfg)
	defer bootstrap.DB().DB.Close()
	for _, name := range names {
		if _, err := bootstrap.DB().Exec("DROP DATABASE IF EXISTS `" + name + "`"); err != nil {
			return fmt.Errorf("drop isolated token TTL database %s: %w", name, err)
		}
	}
	return nil
}
