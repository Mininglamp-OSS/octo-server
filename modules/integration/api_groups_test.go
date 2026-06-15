package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/modules/botfather"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exchangeTeamGroupKey runs the OIDC exchange to mint a uk_ key for (subject, spaceID).
func exchangeTeamGroupKey(t *testing.T, route http.Handler, mp *oidc.MockProvider, subject, spaceID string) string {
	t.Helper()
	idToken := mintIntegrationIDToken(t, mp, subject)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/exchange", idToken, map[string]interface{}{
		"space_id": spaceID,
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		APIKey string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.APIKey)
	return resp.APIKey
}

type teamGroupMemberRow struct {
	UID      string `db:"uid"`
	Role     int    `db:"role"`
	Robot    int    `db:"robot"`
	BotAdmin int    `db:"bot_admin"`
}

func queryTeamGroupMembers(t *testing.T, ctx *config.Context, groupNo string) []teamGroupMemberRow {
	t.Helper()
	var rows []teamGroupMemberRow
	_, err := ctx.DB().SelectBySql(
		"SELECT uid, role, robot, bot_admin FROM group_member WHERE group_no=? AND is_deleted=0 ORDER BY role DESC, uid ASC",
		groupNo,
	).Load(&rows)
	require.NoError(t, err)
	return rows
}

func TestIntegrationCreateTeamGroupSuccess(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-create-ok"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	botA := "bot_" + util.GenerUUID()[:8]
	botB := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, botA, "")
	seedOwnBot(t, ctx, uid, spaceID, botB, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{botA, botB},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp createGroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.GroupID)
	assert.Equal(t, spaceID, resp.SpaceID)
	assert.Equal(t, uid, resp.OwnerUserID)
	assert.Equal(t, "团队群", resp.Name)
	assert.ElementsMatch(t, []string{botA, botB}, resp.MemberRobotIDs)
	_, err := time.Parse(time.RFC3339, resp.CreatedAt)
	assert.NoError(t, err, "created_at must be RFC3339, got %q", resp.CreatedAt)

	// 落库断言：owner=creator role，bot role=common + robot=1，无 bot_admin。
	members := queryTeamGroupMembers(t, ctx, resp.GroupID)
	byUID := make(map[string]teamGroupMemberRow, len(members))
	for _, m := range members {
		byUID[m.UID] = m
		assert.Equal(t, 0, m.BotAdmin, "no member should be bot_admin, uid=%s", m.UID)
	}
	require.Contains(t, byUID, uid)
	assert.Equal(t, group.MemberRoleCreator, byUID[uid].Role)
	for _, b := range []string{botA, botB} {
		require.Contains(t, byUID, b, "bot %s must be a real group member", b)
		assert.Equal(t, group.MemberRoleCommon, byUID[b].Role)
		assert.Equal(t, 1, byUID[b].Robot, "bot %s must have robot=1", b)
	}
}

func TestIntegrationCreateTeamGroupNameBoundary(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-name-bound"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	// 50 runes → OK.
	name50 := strings.Repeat("名", 50)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             name50,
		"member_robot_ids": []string{bot},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp createGroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, name50, resp.Name)

	// 51 runes → 400 param invalid (field name).
	name51 := strings.Repeat("名", 51)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             name51,
		"member_robot_ids": []string{bot},
	}))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "err.shared.param.invalid", decodeErrCode(t, w))
}

func TestIntegrationCreateTeamGroupEmptyAndDedupMembers(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-members"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	// empty → 400.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{},
	}))
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "err.shared.param.invalid", decodeErrCode(t, w))

	// duplicates collapse to one member.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{bot, bot, " " + bot + " "},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp createGroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []string{bot}, resp.MemberRobotIDs)
}

func TestIntegrationCreateTeamGroupBotNotOwnedReturns404(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-bot-404"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	ownBot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, ownBot, "")

	// A bot owned by a different user, in a different space → not in caller's set.
	otherUID := seedIntegrationUser(t, ctx, mp.Issuer, "sub-other")
	otherSpace := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, otherUID, otherSpace, "Other", 1, "2026-01-01 10:00:00")
	foreignBot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, otherUID, otherSpace, foreignBot, "")

	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	for _, badBot := range []string{foreignBot, "bot_does_not_exist"} {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
			"name":             "团队群",
			"member_robot_ids": []string{ownBot, badBot},
		}))
		require.Equal(t, http.StatusNotFound, w.Code, "bad bot %q: %s", badBot, w.Body.String())
		assert.Equal(t, "err.shared.not_found", decodeErrCode(t, w))
	}
}

func TestIntegrationCreateTeamGroupOwnerNotSpaceMemberReturns403(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-403"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	// Owner is a member at exchange time (so the key issues), then removed before create.
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	_, err := ctx.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", spaceID, uid).Exec()
	require.NoError(t, err)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{bot},
	}))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "err.shared.auth.forbidden", decodeErrCode(t, w))
}

func TestIntegrationCreateTeamGroupRejectsRobotOwner(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-robot-owner"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	// Flip the OIDC-linked account to a robot. AuthByKey only checks account activity (not
	// the robot flag), so the key still authenticates — the owner-must-be-human guard in
	// createGroup is what must reject it.
	_, err := ctx.DB().Update("user").Set("robot", 1).Where("uid=?", uid).Exec()
	require.NoError(t, err)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{bot},
	}))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Equal(t, "err.shared.auth.forbidden", decodeErrCode(t, w))
}

func TestIntegrationCreateTeamGroupRejectsForeignClientKey(t *testing.T) {
	route, ctx, _ := setupIntegrationAPITest(t)
	// A key scoped to a different client (here botfather) must be rejected by the
	// middleware — the uk_ endpoints only accept the registered integration client.
	botfatherUID := "u_" + util.GenerUUID()[:8]
	insertIntegrationBareUser(t, ctx, botfatherUID)
	botfatherKey, err := botfather.NewUserAPIKeyService(ctx).GetOrCreate(botfatherUID, "", "")
	require.NoError(t, err)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", botfatherKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{"bot_x"},
	}))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Equal(t, "err.shared.auth.token_invalid", decodeErrCode(t, w))
}

func TestIntegrationCreateTeamGroupIdempotency(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-idem"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	doCreate := func(idemKey, name string) *httptest.ResponseRecorder {
		req := integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
			"name":             name,
			"member_robot_ids": []string{bot},
		})
		if idemKey != "" {
			req.Header.Set(idempotencyKeyHeader, idemKey)
		}
		w := httptest.NewRecorder()
		route.ServeHTTP(w, req)
		return w
	}

	// Same key + same payload → replay same group_id.
	idemKey := "idem-" + util.GenerUUID()[:8]
	w1 := doCreate(idemKey, "团队群")
	require.Equal(t, http.StatusOK, w1.Code, w1.Body.String())
	var r1 createGroupResp
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &r1))

	w2 := doCreate(idemKey, "团队群")
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var r2 createGroupResp
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &r2))
	assert.Equal(t, r1.GroupID, r2.GroupID, "same idempotency key + payload must replay the same group")

	// Same key + different payload → 409 terminal conflict (not retryable).
	w3 := doCreate(idemKey, "另一个名字")
	require.Equal(t, http.StatusConflict, w3.Code, w3.Body.String())
	assert.Equal(t, "err.server.integration.idempotency_conflict", decodeErrCode(t, w3))

	// In-flight (pending sentinel present) → 409 with Retry-After (retryable).
	inflightKey := "idem-inflight-" + util.GenerUUID()[:8]
	pending, _ := json.Marshal(idemRecord{State: idemStatePending, SHA: "stale"})
	rkey := teamGroupIdemRedisKey(defaultClientID, uid, spaceID, inflightKey)
	require.NoError(t, sharedIntegrationRateRedis(ctx.GetConfig()).Set(rkey, pending, time.Minute).Err())
	w4 := doCreate(inflightKey, "团队群")
	require.Equal(t, http.StatusConflict, w4.Code, w4.Body.String())
	assert.Equal(t, "err.server.integration.idempotency_in_flight", decodeErrCode(t, w4))
	assert.NotEmpty(t, w4.Header().Get("Retry-After"))
}

func TestIntegrationTeamGroupExists(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-exists"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceID := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceID, "Team", 1, "2026-01-01 10:00:00")
	bot := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceID, bot, "")
	apiKey := exchangeTeamGroupKey(t, route, mp, subject, spaceID)

	// Create a group to probe.
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", apiKey, map[string]interface{}{
		"name":             "团队群",
		"member_robot_ids": []string{bot},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created createGroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	groupNo := created.GroupID

	checkExists := func(gno string) groupExistsResp {
		ww := httptest.NewRecorder()
		route.ServeHTTP(ww, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/groups/"+gno, apiKey, nil))
		require.Equal(t, http.StatusOK, ww.Code, ww.Body.String())
		var r groupExistsResp
		require.NoError(t, json.Unmarshal(ww.Body.Bytes(), &r))
		assert.Equal(t, gno, r.GroupID)
		return r
	}

	// normal + owner active member → exists:true.
	assert.True(t, checkExists(groupNo).Exists)

	// unknown group → exists:false (no 404).
	assert.False(t, checkExists("grp_does_not_exist").Exists)

	// owner blacklisted (member status != normal) → exists:false.
	_, err := ctx.DB().Update("group_member").Set("status", 2).
		Where("group_no=? AND uid=?", groupNo, uid).Exec()
	require.NoError(t, err)
	assert.False(t, checkExists(groupNo).Exists)

	// restore membership to normal (status=1), then disband the group → exists:false.
	_, err = ctx.DB().Update("group_member").Set("status", 1).
		Where("group_no=? AND uid=?", groupNo, uid).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("group").Set("status", group.GroupStatusDisband).
		Where("group_no=?", groupNo).Exec()
	require.NoError(t, err)
	assert.False(t, checkExists(groupNo).Exists)
}

// TestIntegrationTeamGroupExistsIsSpaceScoped verifies the existence check stays inside the
// uk_ key's space binding: a key bound to space A must not confirm a group that lives in
// space B, even though the same user is an active member of that space-B group.
func TestIntegrationTeamGroupExistsIsSpaceScoped(t *testing.T) {
	route, ctx, mp := setupIntegrationAPITest(t)
	subject := "sub-space-scope"
	uid := seedIntegrationUser(t, ctx, mp.Issuer, subject)
	spaceA := "sp_" + util.GenerUUID()[:8]
	spaceB := "sp_" + util.GenerUUID()[:8]
	seedSpaceMembership(t, ctx, uid, spaceA, "TeamA", 1, "2026-01-01 10:00:00")
	seedSpaceMembership(t, ctx, uid, spaceB, "TeamB", 1, "2026-01-02 10:00:00")
	botB := "bot_" + util.GenerUUID()[:8]
	seedOwnBot(t, ctx, uid, spaceB, botB, "")

	// Create a group in space B with the space-B key.
	keyB := exchangeTeamGroupKey(t, route, mp, subject, spaceB)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, integrationRequest(t, http.MethodPost, "/v1/integrations/oidc/groups", keyB, map[string]interface{}{
		"name":             "团队群B",
		"member_robot_ids": []string{botB},
	}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created createGroupResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	groupB := created.GroupID

	exists := func(apiKey string) bool {
		ww := httptest.NewRecorder()
		route.ServeHTTP(ww, integrationRequest(t, http.MethodGet, "/v1/integrations/oidc/groups/"+groupB, apiKey, nil))
		require.Equal(t, http.StatusOK, ww.Code, ww.Body.String())
		var r groupExistsResp
		require.NoError(t, json.Unmarshal(ww.Body.Bytes(), &r))
		return r.Exists
	}

	// Space-A key must NOT see the space-B group, despite the user being an active member.
	keyA := exchangeTeamGroupKey(t, route, mp, subject, spaceA)
	assert.False(t, exists(keyA), "a space-A key must not confirm a space-B group")
	// Space-B key (correct binding) sees it.
	assert.True(t, exists(keyB), "the space-B key must see its own group")
}

// decodeErrCode extracts error.code from the shared i18n error envelope.
func decodeErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env integrationErrEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body: %s", w.Body.String())
	return env.Error.Code
}
