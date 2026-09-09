package robot

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

type avatarCreateResponse struct {
	RobotID          string `json:"robot_id"`
	BotToken         string `json:"bot_token"`
	PublicationState string `json:"publication_state"`
}

func setupAvatarManager(t *testing.T) (*wkhttp.WKHttp, *Manager, map[string]string) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	// The robot package cannot import modules/ai_team without a cycle. This
	// compatibility fixture mirrors the lifecycle columns touched by the shared
	// cleanup path; the ai_team package's own tests exercise its full migration.
	_, err := ctx.DB().Exec(`CREATE TABLE IF NOT EXISTS ai_team_agent (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		space_id VARCHAR(40) NOT NULL,user_uid VARCHAR(40) NOT NULL,bot_id VARCHAR(40) NOT NULL,
		group_no VARCHAR(40) NULL,is_added TINYINT NOT NULL DEFAULT 1,
		container_state TINYINT NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_ai_team_agent_owner (space_id,user_uid,bot_id),
		UNIQUE KEY uk_ai_team_agent_group (group_no)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	require.NoError(t, err)
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	m := NewManager(ctx)
	m.Route(route)

	suffix := util.GenerUUID()[:8]
	spaceID := "avatar_mgr_space_" + suffix
	superUID := "avatar_mgr_super_" + suffix
	adminUID := "avatar_mgr_admin_" + suffix
	memberUID := "avatar_mgr_member_" + suffix
	otherAdminUID := "avatar_mgr_other_admin_" + suffix
	otherSpaceID := "avatar_mgr_other_space_" + suffix
	_, err = ctx.DB().InsertBySql("INSERT INTO space (space_id,name,status) VALUES (?,?,1)", spaceID, spaceID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO space (space_id,name,status) VALUES (?,?,1)", otherSpaceID, otherSpaceID).Exec()
	require.NoError(t, err)
	for _, uid := range []string{superUID, adminUID, memberUID, otherAdminUID} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,0,0)",
			uid, uid, uid, util.GenerUUID()[:8]).Exec()
		require.NoError(t, err)
	}
	_, err = ctx.DB().InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,1,1),(?,?,0,1)",
		spaceID, adminUID, spaceID, memberUID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,1,1)",
		otherSpaceID, otherAdminUID).Exec()
	require.NoError(t, err)

	tokens := map[string]string{
		"super":     "avatar-super-" + suffix,
		"admin":     "avatar-admin-" + suffix,
		"member":    "avatar-member-" + suffix,
		"other":     "avatar-other-admin-" + suffix,
		"space":     spaceID,
		"super_uid": superUID,
		"admin_uid": adminUID,
	}
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+tokens["super"],
		superUID+"@superadmin@"+string(wkhttp.SuperAdmin)))
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+tokens["admin"], adminUID+"@admin"))
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+tokens["member"], memberUID+"@member"))
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+tokens["other"], otherAdminUID+"@admin"))
	t.Cleanup(func() {
		for _, key := range []string{"super", "admin", "member", "other"} {
			_ = ctx.GetRedisConn().Del(ctx.GetConfig().Cache.TokenCachePrefix + tokens[key])
		}
	})
	return route, m, tokens
}

func TestPlatformAvatarEnrollmentCoversExistingAndConcurrentNewSpace(t *testing.T) {
	route, manager, tokens := setupAvatarManager(t)
	spacemod.New(manager.ctx).Route(route)

	created := avatarManagerRequest(t, route, http.MethodPost, "/v1/manager/avatars", tokens["super"], map[string]any{
		"name": "Platform employee", "scope": "platform",
	})
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var avatar avatarCreateResponse
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &avatar))

	var wg sync.WaitGroup
	wg.Add(2)
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		defer wg.Done()
		responses <- avatarManagerRequest(t, route, http.MethodPost,
			"/v1/manager/avatars/"+avatar.RobotID+"/publish", tokens["super"], nil)
	}()
	go func() {
		defer wg.Done()
		responses <- avatarManagerRequest(t, route, http.MethodPost, "/v1/space/create", tokens["super"], map[string]any{
			"name": "Concurrent Avatar Space", "join_mode": 1,
		})
	}()
	wg.Wait()
	close(responses)
	var newSpaceID string
	for response := range responses {
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
		if value, ok := body["space_id"].(string); ok {
			newSpaceID = value
		}
	}
	require.NotEmpty(t, newSpaceID)

	for _, spaceID := range []string{tokens["space"], newSpaceID} {
		var seats int
		require.NoError(t, manager.ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, avatar.RobotID,
		).LoadOne(&seats))
		require.Equal(t, 1, seats, "publication concurrent with Space creation must not miss enrollment")
	}
	var groupMemberships, projectMemberships int
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE uid=? AND is_deleted=0", avatar.RobotID).LoadOne(&groupMemberships))
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM octo_project_member WHERE uid=? AND status=1", avatar.RobotID).LoadOne(&projectMemberships))
	require.Zero(t, groupMemberships)
	require.Zero(t, projectMemberships)
}

func avatarManagerRequest(t *testing.T, route http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	return w
}

func createSpaceAvatar(t *testing.T, route http.Handler, tokens map[string]string) avatarCreateResponse {
	t.Helper()
	w := avatarManagerRequest(t, route, http.MethodPost, "/v1/manager/avatars", tokens["admin"], map[string]any{
		"name": "Digital employee", "description": "managed", "scope": "space", "space_id": tokens["space"],
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response avatarCreateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotEmpty(t, response.RobotID)
	require.NotEmpty(t, response.BotToken)
	require.Equal(t, botpolicy.Draft, response.PublicationState)
	return response
}

func TestAvatarManagerAuthorizationAndTokenLifecycle(t *testing.T) {
	route, manager, tokens := setupAvatarManager(t)
	manager.cleanupConnectionFn = func(string) error { return nil }
	created := createSpaceAvatar(t, route, tokens)

	var row struct {
		Kind       string `db:"kind"`
		CreatorUID string `db:"creator_uid"`
		CreatedBy  string `db:"created_by"`
		Status     int    `db:"status"`
	}
	require.NoError(t, manager.ctx.DB().Select("kind", "creator_uid", "created_by", "status").From("robot").
		Where("robot_id=?", created.RobotID).LoadOne(&row))
	require.Equal(t, "avatar", row.Kind)
	require.Empty(t, row.CreatorUID, "an Avatar must never impersonate its administrator as owner")
	require.Equal(t, tokens["admin_uid"], row.CreatedBy)
	require.Zero(t, row.Status)

	denied := avatarManagerRequest(t, route, http.MethodGet, "/v1/manager/avatars/"+created.RobotID+"/token", tokens["member"], nil)
	require.NotEqual(t, http.StatusOK, denied.Code, denied.Body.String())
	otherSpaceDenied := avatarManagerRequest(t, route, http.MethodGet, "/v1/manager/avatars/"+created.RobotID+"/token", tokens["other"], nil)
	require.NotEqual(t, http.StatusOK, otherSpaceDenied.Code, otherSpaceDenied.Body.String())
	revealed := avatarManagerRequest(t, route, http.MethodGet, "/v1/manager/avatars/"+created.RobotID+"/token", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, revealed.Code, revealed.Body.String())
	require.Contains(t, revealed.Body.String(), created.BotToken)

	published := avatarManagerRequest(t, route, http.MethodPost, "/v1/manager/avatars/"+created.RobotID+"/publish", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, published.Code, published.Body.String())
	var seatCount int
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		tokens["space"], created.RobotID).LoadOne(&seatCount))
	require.Equal(t, 1, seatCount)

	rotated := avatarManagerRequest(t, route, http.MethodPost, "/v1/manager/avatars/"+created.RobotID+"/token/rotate", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, rotated.Code, rotated.Body.String())
	var tokenResp map[string]string
	require.NoError(t, json.Unmarshal(rotated.Body.Bytes(), &tokenResp))
	require.NotEmpty(t, tokenResp["bot_token"])
	require.NotEqual(t, created.BotToken, tokenResp["bot_token"])
	old, err := manager.db.queryRobotByBotToken(created.BotToken)
	require.NoError(t, err)
	require.Nil(t, old, "the old token must stop authenticating as soon as rotation commits")
}

func TestAvatarCannotUseLegacyRobotManagementRoutes(t *testing.T) {
	route, manager, tokens := setupAvatarManager(t)
	manager.cleanupConnectionFn = func(string) error { return nil }
	created := createSpaceAvatar(t, route, tokens)
	require.Equal(t, http.StatusOK, avatarManagerRequest(t, route, http.MethodPost,
		"/v1/manager/avatars/"+created.RobotID+"/publish", tokens["admin"], nil).Code)

	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/v1/manager/robots/" + created.RobotID, nil},
		{http.MethodPut, "/v1/manager/robots/" + created.RobotID, map[string]any{"description": "bypass"}},
		{http.MethodDelete, "/v1/manager/robots/" + created.RobotID, nil},
		{http.MethodPost, "/v1/manager/robots/" + created.RobotID + "/revoke_token", nil},
		{http.MethodPut, "/v1/manager/robot/status/" + created.RobotID + "/0", nil},
		{http.MethodGet, "/v1/manager/robot/menus?robot_id=" + created.RobotID, nil},
		{http.MethodDelete, "/v1/manager/robot/" + created.RobotID + "/1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			response := avatarManagerRequest(t, route, tc.method, tc.path, tokens["super"], tc.body)
			require.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
		})
	}

	identity, err := botpolicy.Lookup(manager.ctx.DB(), created.RobotID)
	require.NoError(t, err)
	require.True(t, identity.ActiveAvatar(), "legacy management attempts must not change Avatar lifecycle state")
	var stored struct {
		BotToken    string `db:"bot_token"`
		Description string `db:"description"`
	}
	require.NoError(t, manager.ctx.DB().Select("bot_token", "description").From("robot").
		Where("robot_id=?", created.RobotID).LoadOne(&stored))
	require.Equal(t, created.BotToken, stored.BotToken)
	require.Equal(t, "managed", stored.Description)
}

func TestAvatarLifecycleFailureRetriesWithoutRestoringRelationships(t *testing.T) {
	route, manager, tokens := setupAvatarManager(t)
	created := createSpaceAvatar(t, route, tokens)
	manager.cleanupConnectionFn = func(string) error { return nil }
	require.Equal(t, http.StatusOK, avatarManagerRequest(t, route, http.MethodPost,
		"/v1/manager/avatars/"+created.RobotID+"/publish", tokens["admin"], nil).Code)

	_, err := manager.ctx.DB().InsertBySql("INSERT INTO friend (uid,to_uid,is_deleted) VALUES (?,?,0)", tokens["admin_uid"], created.RobotID).Exec()
	require.NoError(t, err)
	_, err = manager.ctx.DB().InsertBySql(`INSERT INTO ai_team_agent
		(space_id,user_uid,bot_id,is_added,container_state) VALUES (?,?,?,1,0)`,
		tokens["space"], tokens["admin_uid"], created.RobotID).Exec()
	require.NoError(t, err)

	manager.closeSeatsFn = func(_ *config.Context, _, _, _ string) ([]string, error) {
		return nil, errors.New("injected seat cleanup failure")
	}
	unpublished := avatarManagerRequest(t, route, http.MethodPost,
		"/v1/manager/avatars/"+created.RobotID+"/unpublish", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, unpublished.Code, unpublished.Body.String())

	identity, err := botpolicy.Lookup(manager.ctx.DB(), created.RobotID)
	require.NoError(t, err)
	require.False(t, identity.ActiveAvatar(), "external cleanup failure must never restore authority")
	require.Equal(t, botpolicy.Unpublished, identity.PublicationState)
	require.Equal(t, 1, identity.LifecyclePending)
	var activeFriend, activeAgent, pendingJobs int
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM friend WHERE to_uid=? AND is_deleted=0", created.RobotID).LoadOne(&activeFriend))
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM ai_team_agent WHERE bot_id=? AND is_added=1", created.RobotID).LoadOne(&activeAgent))
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM avatar_cleanup_job WHERE robot_id=? AND state='pending'", created.RobotID).LoadOne(&pendingJobs))
	require.Zero(t, activeFriend)
	require.Zero(t, activeAgent)
	require.Equal(t, 1, pendingJobs)

	manager.closeSeatsFn = func(_ *config.Context, uid, _, _ string) ([]string, error) {
		_, updateErr := manager.ctx.DB().Update("space_member").Set("status", 0).Where("uid=?", uid).Exec()
		return []string{tokens["space"]}, updateErr
	}
	retried := avatarManagerRequest(t, route, http.MethodPost,
		"/v1/manager/avatars/"+created.RobotID+"/cleanup/retry", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, retried.Code, retried.Body.String())
	identity, err = botpolicy.Lookup(manager.ctx.DB(), created.RobotID)
	require.NoError(t, err)
	require.Zero(t, identity.LifecyclePending)

	republished := avatarManagerRequest(t, route, http.MethodPost,
		"/v1/manager/avatars/"+created.RobotID+"/publish", tokens["admin"], nil)
	require.Equal(t, http.StatusOK, republished.Code, republished.Body.String())
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM friend WHERE to_uid=? AND is_deleted=0", created.RobotID).LoadOne(&activeFriend))
	require.NoError(t, manager.ctx.DB().SelectBySql("SELECT COUNT(*) FROM ai_team_agent WHERE bot_id=? AND is_added=1", created.RobotID).LoadOne(&activeAgent))
	require.Zero(t, activeFriend, "republish must not restore old DM relationships")
	require.Zero(t, activeAgent, "republish must not restore old AI Team membership")
}
