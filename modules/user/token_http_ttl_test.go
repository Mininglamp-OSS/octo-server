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
	server, ctx := newTokenHTTPTestServer(t)
	t.Run("manager login issues a finite token", func(t *testing.T) {
		testManagerLoginIssuesFiniteTokenOverHTTP(t, server, ctx)
	})
	t.Run("nickname update preserves the deadline", func(t *testing.T) {
		testNicknameUpdatePreservesFiniteTokenDeadlineOverHTTP(t, server, ctx)
	})
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

func newTokenHTTPTestServer(t *testing.T) (*libserver.Server, *config.Context) {
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
	ctx := config.NewContext(cfg)

	server := libserver.New(ctx)
	server.GetRoute().UseGin(ctx.Tracer().GinMiddle())
	ctx.SetHttpRoute(server.GetRoute())
	require.NoError(t, module.Setup(ctx), "set up modules in isolated token TTL database")
	return server, ctx
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
