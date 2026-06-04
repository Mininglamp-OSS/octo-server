package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	libwkhttp "github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/modules/oidc"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupIntegrationAPITest(t *testing.T) (http.Handler, *config.Context, *oidc.MockProvider) {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "fedcba9876543210fedcba9876543210")

	mp := oidc.NewMockProvider(t)
	rtKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", mp.Issuer)
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_ID", mp.ClientID)
	t.Setenv("DM_OIDC_PROVIDER_CLIENT_SECRET", "test-secret")
	t.Setenv("DM_OIDC_PROVIDER_REDIRECT_URI", "https://octo.example/callback")
	t.Setenv("DM_OIDC_RT_ENC_KEY", rtKey)
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_RPS", "1000")
	t.Setenv("DM_INTEGRATION_IP_RATELIMIT_BURST", "10000")

	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	seedIntegrationClient(t, ctx, defaultClientID, 1)
	return s.GetRoute(), ctx, mp
}

func seedIntegrationClient(t *testing.T, ctx *config.Context, clientID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO integration_client (client_id, name, status) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE status=VALUES(status), name=VALUES(name)",
		clientID, clientID, status,
	).Exec()
	require.NoError(t, err)
}

func seedIntegrationUser(t *testing.T, ctx *config.Context, issuer, subject string) string {
	t.Helper()
	uid := "u_" + util.GenerUUID()[:8]
	insertIntegrationBareUser(t, ctx, uid)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user_oidc_identity (uid, issuer, subject, email, email_verified) VALUES (?, ?, ?, ?, 1)",
		uid, issuer, subject, uid+"@example.com",
	).Exec()
	require.NoError(t, err)
	return uid
}

func insertIntegrationBareUser(t *testing.T, ctx *config.Context, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user (uid, name, username, short_no, status) VALUES (?, ?, ?, ?, 1)",
		uid, "User "+uid, uid, "sn_"+uid,
	).Exec()
	require.NoError(t, err)
}

func seedSpaceMembership(t *testing.T, ctx *config.Context, uid, spaceID, name string, role int, joinedAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?)",
		spaceID, name, joinedAt, joinedAt,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)",
		spaceID, uid, role, joinedAt, joinedAt,
	).Exec()
	require.NoError(t, err)
}

func seedOwnBot(t *testing.T, ctx *config.Context, uid, spaceID, robotID, boundAgentRef string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user (uid, name, username, short_no, robot, status) VALUES (?, ?, ?, ?, 1, 1)",
		robotID, "Bot "+robotID, robotID, "sn_"+robotID,
	).Exec()
	require.NoError(t, err)
	boundAtExpr := "NULL"
	args := []interface{}{robotID, robotID, uid, "bf_" + robotID, boundAgentRef}
	if boundAgentRef != "" {
		boundAtExpr = "NOW()"
	}
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, username, creator_uid, bot_token, status, bound_agent_ref, bound_at) VALUES (?, ?, ?, ?, 1, ?, "+boundAtExpr+")",
		args...,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, robotID,
	).Exec()
	require.NoError(t, err)
}

func mintIntegrationIDToken(t *testing.T, mp *oidc.MockProvider, subject string) string {
	t.Helper()
	mp.PrepUser(subject, map[string]interface{}{"email": subject + "@example.com", "email_verified": true})
	code := "code_" + util.GenerUUID()
	mp.PrepCode(code, subject, "nonce")
	client, err := oidc.NewClient(context.Background(), oidc.ClientConfig{
		Issuer:       mp.Issuer,
		ClientID:     mp.ClientID,
		ClientSecret: "test-secret",
		RedirectURI:  "https://octo.example/callback",
		Scopes:       []string{"openid", "profile", "email"},
		ClockSkew:    time.Minute,
		HTTPTimeout:  5 * time.Second,
	})
	require.NoError(t, err)
	tok, err := client.Exchange(context.Background(), code, "verifier")
	require.NoError(t, err)
	raw, ok := tok.Extra("id_token").(string)
	require.True(t, ok)
	require.NotEmpty(t, raw)
	return raw
}

func integrationRequest(t *testing.T, method, path, token string, body interface{}) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

type integrationErrEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func TestOIDCSpacesAndExchangeFlow(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-flow"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceA, "Research", 2, "2026-01-01 10:00:00")
	seedSpaceMembership(t, ctx, uid, spaceB, "Sandbox", 0, "2026-01-02 10:00:00")
	seedOwnBot(t, ctx, uid, spaceA, "bot_"+util.GenerUUID()[:8], "")
	seedOwnBot(t, ctx, uid, spaceB, "busy_"+util.GenerUUID()[:8], "octopush:agent_busy")
	idToken := mintIntegrationIDToken(t, mp, subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var spacesResp struct {
		UID      string `json:"uid"`
		ClientID string `json:"client_id"`
		Spaces   []struct {
			SpaceID         string `json:"space_id"`
			Name            string `json:"name"`
			Role            int    `json:"role"`
			MemberCount     int    `json:"member_count"`
			IsDefault       bool   `json:"is_default"`
			HasAvailableBot bool   `json:"has_available_bot"`
		} `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spacesResp))
	assert.Equal(t, uid, spacesResp.UID)
	assert.Equal(t, defaultClientID, spacesResp.ClientID)
	require.Len(t, spacesResp.Spaces, 2)
	bySpace := map[string]bool{}
	for _, sp := range spacesResp.Spaces {
		bySpace[sp.SpaceID] = sp.HasAvailableBot
		if sp.SpaceID == spaceA {
			assert.True(t, sp.IsDefault)
		}
	}
	assert.True(t, bySpace[spaceA])
	assert.False(t, bySpace[spaceB])

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id":     spaceA,
		"include_bots": true,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		UID       string `json:"uid"`
		SpaceID   string `json:"space_id"`
		ClientID  string `json:"client_id"`
		APIKey    string `json:"api_key"`
		SpaceName string `json:"space_name"`
		Bots      []struct {
			RobotID  string `json:"robot_id"`
			BotToken string `json:"bot_token"`
		} `json:"bots"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))
	assert.Equal(t, uid, exchangeResp.UID)
	assert.Equal(t, spaceA, exchangeResp.SpaceID)
	assert.Equal(t, defaultClientID, exchangeResp.ClientID)
	assert.Equal(t, "Research", exchangeResp.SpaceName)
	assert.Contains(t, exchangeResp.APIKey, botfather.UserAPIKeyPrefix)
	require.Len(t, exchangeResp.Bots, 1)
	assert.Empty(t, exchangeResp.Bots[0].BotToken)
}

func TestOIDCSpacesWithoutMembershipReturnsEmptyArray(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-no-spaces"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	idToken := mintIntegrationIDToken(t, mp, subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var spacesResp struct {
		UID    string          `json:"uid"`
		Spaces json.RawMessage `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spacesResp))
	assert.Equal(t, uid, spacesResp.UID)
	assert.JSONEq(t, `[]`, string(spacesResp.Spaces))
}

func TestOIDCSpacesAcceptsCaseInsensitiveBearerScheme(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-lower-bearer"
	seedIntegrationUser(t, ctx, mp.Issuer, subject)
	idToken := mintIntegrationIDToken(t, mp, subject)

	req := integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", "", nil)
	req.Header.Set("Authorization", "bearer "+idToken)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestOIDCExchangeRejectsInactiveLinkedUser(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		isDestroy int
	}{
		{name: "disabled", status: 0, isDestroy: 0},
		{name: "destroyed", status: 1, isDestroy: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, ctx, mp := setupIntegrationAPITest(t)
			subject := "sub-inactive-" + tc.name
			uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
			spaceID := "sp_" + util.GenerUUID()[:8]
			seedSpaceMembership(t, ctx, uid, spaceID, "Inactive", 2, "2026-01-01 10:00:00")
			_, err := ctx.DB().Update("user").
				Set("status", tc.status).
				Set("is_destroy", tc.isDestroy).
				Where("uid=?", uid).
				Exec()
			require.NoError(t, err)
			idToken := mintIntegrationIDToken(t, mp, subject)

			w := httptest.NewRecorder()
			route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
				"space_id": spaceID,
			}))

			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var env integrationErrEnvelope
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
			assert.Equal(t, "err.server.integration.user_not_linked", env.Error.Code)

			var active int
			err = ctx.DB().SelectBySql(
				"SELECT COUNT(*) FROM user_api_key WHERE uid=? AND space_id=? AND client_id=? AND status=1",
				uid, spaceID, defaultClientID,
			).LoadOne(&active)
			require.NoError(t, err)
			assert.Equal(t, 0, active)
		})
	}
}

func TestNewConfigBranches(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)

	t.Setenv("DM_OIDC_ENABLED", "false")
	disabled := New(ctx)
	assert.Nil(t, disabled.oidcClient)

	t.Setenv("DM_OIDC_ENABLED", "true")
	t.Setenv("DM_OIDC_RT_ENC_KEY", "not-base64")
	badConfig := New(ctx)
	assert.Nil(t, badConfig.oidcClient)

	rtKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("DM_OIDC_RT_ENC_KEY", rtKey)
	t.Setenv("DM_OIDC_PROVIDER_ISSUER", "://bad")
	badDiscovery := New(ctx)
	assert.Nil(t, badDiscovery.oidcClient)
}

func TestIntegrationDBMissingClientAndBlankSpace(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	db := newIntegrationDB(ctx)

	enabled, err := db.isClientEnabled("missing-client")
	require.NoError(t, err)
	assert.False(t, enabled)

	name, err := db.queryActiveSpaceName("")
	require.NoError(t, err)
	assert.Empty(t, name)

	available, err := db.queryAvailableBotSpaces("", nil)
	require.NoError(t, err)
	assert.Empty(t, available)
}

func TestManagerIntegrationClientUpsertRequiresSuperAdmin(t *testing.T) {
	route, ctx, _ := setupIntegrationAPITest(t)
	_, err := ctx.DB().DeleteFrom("integration_client").Where("client_id=?", defaultClientID).Exec()
	require.NoError(t, err)
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(libwkhttp.Admin),
	))

	w := httptest.NewRecorder()
	req := integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"name":   "Octopush",
		"status": 1,
	})
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, "non-superAdmin must not create integration clients")

	enabled, err := newIntegrationDB(ctx).isClientEnabled(defaultClientID)
	require.NoError(t, err)
	assert.False(t, enabled)
}

func TestManagerIntegrationClientUpsertAndDisableRevokesKeys(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(libwkhttp.SuperAdmin),
	))

	_, err := ctx.DB().DeleteFrom("integration_client").Where("client_id=?", defaultClientID).Exec()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req := integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"name":   "Octopush",
		"status": 1,
	})
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var clientResp struct {
		ClientID string `json:"client_id"`
		Name     string `json:"name"`
		Status   int    `json:"status"`
		Enabled  bool   `json:"enabled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &clientResp))
	assert.Equal(t, defaultClientID, clientResp.ClientID)
	assert.Equal(t, "Octopush", clientResp.Name)
	assert.Equal(t, 1, clientResp.Status)
	assert.True(t, clientResp.Enabled)

	subject := "sub-manager-disable"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Managed", 2, "2026-01-01 10:00:00")
	idToken := mintIntegrationIDToken(t, mp, subject)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))
	require.NotEmpty(t, exchangeResp.APIKey)

	w = httptest.NewRecorder()
	req = integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"name":   "Octopush",
		"status": 0,
	})
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &clientResp))
	assert.Equal(t, 0, clientResp.Status)
	assert.False(t, clientResp.Enabled)

	auth, err := botfather.NewUserAPIKeyService(ctx).AuthByKey(exchangeResp.APIKey)
	require.NoError(t, err)
	assert.Nil(t, auth, "disabling the integration client must immediately revoke active octopush uk_ keys")
}

func TestManagerIntegrationClientEnableRequiresUserAPIKeySecret(t *testing.T) {
	route, ctx, _ := setupIntegrationAPITest(t)
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(libwkhttp.SuperAdmin),
	))
	_, err := ctx.DB().DeleteFrom("integration_client").Where("client_id=?", defaultClientID).Exec()
	require.NoError(t, err)
	t.Setenv("OCTO_USER_API_KEY_SECRET", "")

	w := httptest.NewRecorder()
	req := integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"name":   "Octopush",
		"status": 1,
	})
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())

	enabled, err := newIntegrationDB(ctx).isClientEnabled(defaultClientID)
	require.NoError(t, err)
	assert.False(t, enabled)
}

func TestOIDCExchangeIssuanceSerializesWithClientDisable(t *testing.T) {
	_, ctx, _ := setupIntegrationAPITest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertIntegrationBareUser(t, ctx, uid)
	svc := botfather.NewUserAPIKeyService(ctx)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.Update("integration_client").
		Set("status", 0).
		Where("client_id=?", defaultClientID).
		Exec()
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		key, issueErr := svc.GetOrCreateForEnabledIntegrationClient(uid, spaceID, defaultClientID)
		if issueErr == nil && key != "" {
			done <- fmt.Errorf("issued key while disable transaction held client lock: %s", key)
			return
		}
		done <- issueErr
	}()

	select {
	case issueErr := <-done:
		require.Failf(t, "issuance completed before disable committed", "err=%v", issueErr)
	case <-time.After(150 * time.Millisecond):
	}

	_, err = tx.Update("user_api_key").
		Set("status", 0).
		Set("revoked_at", time.Now()).
		Where("client_id=? AND status=1", defaultClientID).
		Exec()
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	committed = true

	err = <-done
	require.Error(t, err)
	assert.True(t, errors.Is(err, botfather.ErrIntegrationClientDisabled), "got %v", err)

	var active int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM user_api_key WHERE uid=? AND space_id=? AND client_id=? AND status=1",
		uid, spaceID, defaultClientID,
	).LoadOne(&active)
	require.NoError(t, err)
	assert.Equal(t, 0, active, "disable/exchange race must not leave an active key")

	seedIntegrationClient(t, ctx, defaultClientID, 1)
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM user_api_key WHERE uid=? AND space_id=? AND client_id=? AND status=1",
		uid, spaceID, defaultClientID,
	).LoadOne(&active)
	require.NoError(t, err)
	assert.Equal(t, 0, active, "re-enabling the client must not resurrect a race-issued key")
}

func TestManagerIntegrationClientValidationAndDefaultName(t *testing.T) {
	route, ctx, _ := setupIntegrationAPITest(t)
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(libwkhttp.SuperAdmin),
	))

	req := httptest.NewRequest(http.MethodPut, "/v1/manager/integrations/oidc/client", bytes.NewBufferString("{"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", testutil.Token)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	req = integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{})
	req.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	req = integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"status": 2,
	})
	req.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	req = integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"status": 1,
	})
	req.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		ClientID string `json:"client_id"`
		Name     string `json:"name"`
		Status   int    `json:"status"`
		Enabled  bool   `json:"enabled"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, defaultClientID, resp.ClientID)
	assert.Equal(t, defaultClientName, resp.Name)
	assert.Equal(t, 1, resp.Status)
	assert.True(t, resp.Enabled)

	req = integrationRequest(t, http.MethodPut, "/v1/manager/integrations/oidc/client", "", map[string]interface{}{
		"name":   strings.Repeat("x", 101),
		"status": 1,
	})
	req.Header.Set("token", testutil.Token)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestOIDCBindingRevokesUserAPIKey(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-revoke"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Research", 2, "2026-01-01 10:00:00")
	idToken := mintIntegrationIDToken(t, mp, subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))
	require.NotEmpty(t, exchangeResp.APIKey)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodDelete, "/v1/integrations/oidc/binding", exchangeResp.APIKey, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"revoked":true}`, w.Body.String())

	auth, err := botfather.NewUserAPIKeyService(ctx).AuthByKey(exchangeResp.APIKey)
	require.NoError(t, err)
	assert.Nil(t, auth)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/user/bots", exchangeResp.APIKey, nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUserAPIKeyRoutesRejectActiveKeyWhenIntegrationClientDisabled(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-disabled-auth"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Research", 2, "2026-01-01 10:00:00")
	idToken := mintIntegrationIDToken(t, mp, subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))
	require.NotEmpty(t, exchangeResp.APIKey)

	seedIntegrationClient(t, ctx, defaultClientID, 0)
	_, err := ctx.DB().Update("user_api_key").
		Set("status", 1).
		Set("revoked_at", nil).
		Where("client_id=? AND api_key_hash <> ''", defaultClientID).
		Exec()
	require.NoError(t, err)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodDelete, "/v1/integrations/oidc/binding", exchangeResp.APIKey, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)

	auth, err := botfather.NewUserAPIKeyService(ctx).AuthByKey(exchangeResp.APIKey)
	require.NoError(t, err)
	assert.Nil(t, auth, "disabled integration client must make even active uk_ rows unusable")

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/user/bots", exchangeResp.APIKey, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestOIDCAuthErrors(t *testing.T) {
	route, _, _ := setupIntegrationAPITest(t)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", "", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_missing", env.Error.Code)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", "not-a-jwt", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)
}

func TestOIDCAuthRejectsBlankSubject(t *testing.T) {
	route, _, mp := setupIntegrationAPITest(t)
	idToken := mintIntegrationIDToken(t, mp, "")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)
}

func TestOIDCExchangeValidationErrors(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-exchange-errors"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	idToken := mintIntegrationIDToken(t, mp, subject)

	req := httptest.NewRequest(http.MethodPost, "/v1/integrations/oidc/exchange", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer "+idToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.param.invalid", env.Error.Code)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{}))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.param.invalid", env.Error.Code)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": "sp_missing",
	}))
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.not_found", env.Error.Code)

	spaceID := "sp_" + util.GenerUUID()[:8]
	otherUID := "u_" + util.GenerUUID()[:8]
	insertIntegrationBareUser(t, ctx, otherUID)
	seedSpaceMembership(t, ctx, otherUID, spaceID, "Private", 0, "2026-01-01 10:00:00")

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.forbidden", env.Error.Code)
	assert.NotEmpty(t, uid)
}

func TestOIDCBindingRejectsInvalidUserAPIKey(t *testing.T) {
	route, ctx, _ := setupIntegrationAPITest(t)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodDelete, "/v1/integrations/oidc/binding", "", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)

	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodDelete, "/v1/integrations/oidc/binding", botfather.UserAPIKeyPrefix+"missing", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)

	botfatherUID := "u_" + util.GenerUUID()[:8]
	insertIntegrationBareUser(t, ctx, botfatherUID)
	botfatherKey, err := botfather.NewUserAPIKeyService(ctx).GetOrCreate(botfatherUID, "", "")
	require.NoError(t, err)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodDelete, "/v1/integrations/oidc/binding", botfatherKey, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)
	auth, err := botfather.NewUserAPIKeyService(ctx).AuthByKey(botfatherKey)
	require.NoError(t, err)
	require.NotNil(t, auth, "binding endpoint must not revoke botfather-scoped uk_ keys")
}

func TestIntegrationHandlersDefensiveWithoutAuthContext(t *testing.T) {
	route := libwkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.SourceLanguage)))
	it := &Integration{}
	group := route.Group("")
	route.GET("/oidc", it.forceEnglish(), it.oidcAuth(), func(c *libwkhttp.Context) {
		c.ResponseOK()
	})
	route.GET("/spaces", it.forceEnglish(), it.listSpaces)
	route.POST("/exchange", it.forceEnglish(), it.exchange)
	group.DELETE("/binding", it.forceEnglish(), it.deleteBinding)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/oidc"},
		{http.MethodGet, "/spaces"},
		{http.MethodPost, "/exchange"},
		{http.MethodDelete, "/binding"},
	} {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, integrationRequest(t, tc.method, tc.path, "token", nil))
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		var env integrationErrEnvelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
		assert.Equal(t, "err.shared.internal", env.Error.Code)
	}
}

func TestOIDCUserNotLinkedFixedEnglishError(t *testing.T) {
	route, _, mp := setupIntegrationAPITest(t)
	idToken := mintIntegrationIDToken(t, mp, "sub-missing")

	req := integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil)
	req.Header.Set("Accept-Language", "zh-CN")
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.server.integration.user_not_linked", env.Error.Code)
	assert.Equal(t, http.StatusForbidden, env.Error.HTTPStatus)
	assert.Equal(t, "User is not linked to an Octo account.", env.Error.Message)
}

func TestOIDCDisabledClientRevokesIssuedKeys(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-disabled"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Research", 2, "2026-01-01 10:00:00")
	idToken := mintIntegrationIDToken(t, mp, subject)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var exchangeResp struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &exchangeResp))

	seedIntegrationClient(t, ctx, defaultClientID, 0)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, "err.server.integration.disabled", env.Error.Code)

	auth, err := botfather.NewUserAPIKeyService(ctx).AuthByKey(exchangeResp.APIKey)
	require.NoError(t, err)
	assert.Nil(t, auth, fmt.Sprintf("disabled %s must revoke issued uk_", defaultClientID))
}
