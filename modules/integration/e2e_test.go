package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCIntegrationE2E_CreateBindTokenUnbindRevoke(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	srv := httptest.NewServer(route)
	defer srv.Close()

	subject := "sub-e2e"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "E2E Space", 2, "2026-01-01 10:00:00")
	idToken := mintIntegrationIDToken(t, mp, subject)

	status, _, body := doE2EJSON(t, srv, http.MethodGet, "/v1/integrations/oidc/spaces", idToken, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var spacesResp struct {
		UID    string `json:"uid"`
		Spaces []struct {
			SpaceID         string `json:"space_id"`
			HasAvailableBot bool   `json:"has_available_bot"`
		} `json:"spaces"`
	}
	require.NoError(t, json.Unmarshal(body, &spacesResp))
	assert.Equal(t, uid, spacesResp.UID)
	require.Len(t, spacesResp.Spaces, 1)
	assert.Equal(t, spaceID, spacesResp.Spaces[0].SpaceID)
	assert.False(t, spacesResp.Spaces[0].HasAvailableBot)

	status, _, body = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id":     spaceID,
		"include_bots": true,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var exchangeResp struct {
		UID     string `json:"uid"`
		SpaceID string `json:"space_id"`
		APIKey  string `json:"api_key"`
		Bots    []struct {
			RobotID string `json:"robot_id"`
		} `json:"bots"`
	}
	require.NoError(t, json.Unmarshal(body, &exchangeResp))
	assert.Equal(t, uid, exchangeResp.UID)
	assert.Equal(t, spaceID, exchangeResp.SpaceID)
	require.Contains(t, exchangeResp.APIKey, botfather.UserAPIKeyPrefix)
	assert.Empty(t, exchangeResp.Bots)

	description := "created by integration e2e"
	status, _, body = doE2EJSON(t, srv, http.MethodPost, "/v1/user/bots", exchangeResp.APIKey, botfather.CreateBotReq{
		Name:        "Octopush E2E Bot",
		Description: &description,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var createResp botfather.CreateBotResp
	require.NoError(t, json.Unmarshal(body, &createResp))
	require.NotEmpty(t, createResp.RobotID)
	assert.Equal(t, "Octopush E2E Bot", createResp.Name)
	assert.Equal(t, description, createResp.Description)
	require.Contains(t, createResp.BotToken, botfather.BotTokenPrefix)

	agentRef := "octopush:agent_e2e"
	status, _, body = doE2EJSON(t, srv, http.MethodPost, fmt.Sprintf("/v1/user/bots/%s/bind", createResp.RobotID), exchangeResp.APIKey, botfather.BindBotReq{
		AgentRef: agentRef,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var bindResp botfather.BindBotResp
	require.NoError(t, json.Unmarshal(body, &bindResp))
	assert.Equal(t, createResp.RobotID, bindResp.RobotID)
	assert.Equal(t, agentRef, bindResp.BoundAgentRef)
	require.NotNil(t, bindResp.BoundAt)
	assert.NotEmpty(t, *bindResp.BoundAt)

	status, _, body = doE2EJSON(t, srv, http.MethodGet, fmt.Sprintf("/v1/user/bots/%s/token", createResp.RobotID), exchangeResp.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var tokenResp struct {
		RobotID  string `json:"robot_id"`
		BotToken string `json:"bot_token"`
	}
	require.NoError(t, json.Unmarshal(body, &tokenResp))
	assert.Equal(t, createResp.RobotID, tokenResp.RobotID)
	assert.Equal(t, createResp.BotToken, tokenResp.BotToken)

	status, _, body = doE2EJSON(t, srv, http.MethodDelete, fmt.Sprintf("/v1/user/bots/%s/bind", createResp.RobotID), exchangeResp.APIKey, botfather.BindBotReq{
		AgentRef: agentRef,
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var unbindResp map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &unbindResp))
	assert.Equal(t, createResp.RobotID, unbindResp["robot_id"])
	assert.Equal(t, "", unbindResp["bound_agent_ref"])
	assert.Contains(t, unbindResp, "bound_at")
	assert.Nil(t, unbindResp["bound_at"])

	status, _, body = doE2EJSON(t, srv, http.MethodDelete, "/v1/integrations/oidc/binding", exchangeResp.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.JSONEq(t, `{"revoked":true}`, string(body))

	status, _, body = doE2EJSON(t, srv, http.MethodGet, fmt.Sprintf("/v1/user/bots/%s/token", createResp.RobotID), exchangeResp.APIKey, nil)
	assert.Equal(t, http.StatusUnauthorized, status, string(body))
}

func doE2EJSON(t *testing.T, srv *httptest.Server, method, path, bearer string, body interface{}) (int, http.Header, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, respBody
}

// doE2EJSONH is doE2EJSON with extra request headers (e.g. Idempotency-Key).
func doE2EJSONH(t *testing.T, srv *httptest.Server, method, path, bearer string, headers map[string]string, body interface{}) (int, http.Header, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, respBody
}

// TestOIDCIntegrationE2E_TeamGroup drives the full team-group flow over a real HTTP server:
// exchange -> create team group -> existence -> boundary/edge cases -> idempotency -> key
// revocation. Complements the handler-level unit tests with an end-to-end key lifecycle.
func TestOIDCIntegrationE2E_TeamGroup(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	srv := httptest.NewServer(route)
	defer srv.Close()

	subject := "sub-e2e-group"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "E2E Group Space", 2, "2026-01-01 10:00:00")
	botA := "bot_" + util.GenerUUID()[:8]
	botB := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, botA, "")
	seedOwnBot(t, ctx, uid, spaceID, botB, "")
	idToken := mintIntegrationIDToken(t, mp, subject)

	// exchange -> uk_
	status, _, body := doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{"space_id": spaceID})
	require.Equal(t, http.StatusOK, status, string(body))
	var ex struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(body, &ex))
	require.NotEmpty(t, ex.APIKey)

	createBody := map[string]interface{}{"name": "团队群 E2E", "member_robot_ids": []string{botA, botB}}

	// create team group
	status, _, body = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, createBody)
	require.Equal(t, http.StatusOK, status, string(body))
	var created createGroupResp
	require.NoError(t, json.Unmarshal(body, &created))
	require.NotEmpty(t, created.GroupID)
	assert.Equal(t, spaceID, created.SpaceID)
	assert.Equal(t, uid, created.OwnerUserID)
	assert.ElementsMatch(t, []string{botA, botB}, created.MemberRobotIDs)

	// existence: creator sees it
	status, _, body = doE2EJSON(t, srv, http.MethodGet, "/v1/integrations/oidc/groups/"+created.GroupID, ex.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var exists groupExistsResp
	require.NoError(t, json.Unmarshal(body, &exists))
	assert.True(t, exists.Exists)

	// existence: unknown group -> exists:false, still HTTP 200
	status, _, body = doE2EJSON(t, srv, http.MethodGet, "/v1/integrations/oidc/groups/grp_nope", ex.APIKey, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.NoError(t, json.Unmarshal(body, &exists))
	assert.False(t, exists.Exists)

	// existence: blank group_no -> 400
	status, _, _ = doE2EJSON(t, srv, http.MethodGet, "/v1/integrations/oidc/groups/%20", ex.APIKey, nil)
	assert.Equal(t, http.StatusBadRequest, status)

	// boundary: name too long (51 runes) -> 400
	status, _, _ = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, map[string]interface{}{
		"name": strings.Repeat("名", 51), "member_robot_ids": []string{botA},
	})
	assert.Equal(t, http.StatusBadRequest, status)

	// boundary: empty members -> 400
	status, _, _ = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, map[string]interface{}{
		"name": "x", "member_robot_ids": []string{},
	})
	assert.Equal(t, http.StatusBadRequest, status)

	// boundary: foreign/unknown bot -> unified 404
	status, _, _ = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, map[string]interface{}{
		"name": "x", "member_robot_ids": []string{"bot_unknown"},
	})
	assert.Equal(t, http.StatusNotFound, status)

	// idempotency over real HTTP: same key + body replays the same group
	idem := map[string]string{idempotencyKeyHeader: "e2e-idem-" + util.GenerUUID()[:8]}
	status, _, body = doE2EJSONH(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, idem, createBody)
	require.Equal(t, http.StatusOK, status, string(body))
	var r1 createGroupResp
	require.NoError(t, json.Unmarshal(body, &r1))
	status, _, body = doE2EJSONH(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, idem, createBody)
	require.Equal(t, http.StatusOK, status, string(body))
	var r2 createGroupResp
	require.NoError(t, json.Unmarshal(body, &r2))
	assert.Equal(t, r1.GroupID, r2.GroupID, "same key+body replays the same group")

	// idempotency: same key + different body -> 409 conflict
	status, _, _ = doE2EJSONH(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, idem, map[string]interface{}{
		"name": "另一个名字", "member_robot_ids": []string{botA},
	})
	assert.Equal(t, http.StatusConflict, status)

	// key lifecycle edge: revoke the uk_, then both endpoints reject it with 401
	status, _, _ = doE2EJSON(t, srv, http.MethodDelete, "/v1/integrations/oidc/binding", ex.APIKey, nil)
	require.Equal(t, http.StatusOK, status)
	status, _, _ = doE2EJSON(t, srv, http.MethodPost, "/v1/integrations/oidc/groups", ex.APIKey, createBody)
	assert.Equal(t, http.StatusUnauthorized, status, "revoked uk_ must be rejected by create")
	status, _, _ = doE2EJSON(t, srv, http.MethodGet, "/v1/integrations/oidc/groups/"+created.GroupID, ex.APIKey, nil)
	assert.Equal(t, http.StatusUnauthorized, status, "revoked uk_ must be rejected by existence check")
}
