package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/modules/ai_team"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/botpolicy"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

func seedActiveAvatarOnContext(t *testing.T, db *dbr.Session, robotID, token, spaceID string) {
	t.Helper()
	_, err := db.InsertBySql("INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,1,0)",
		robotID, robotID, robotID, util.GenerUUID()[:8]).Exec()
	require.NoError(t, err)
	_, err = db.InsertBySql("INSERT INTO space (space_id,name,status) VALUES (?,?,1)", spaceID, spaceID).Exec()
	require.NoError(t, err)
	_, err = db.InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,0,1)", spaceID, robotID).Exec()
	require.NoError(t, err)
	_, err = db.InsertBySql(`INSERT INTO robot
		(robot_id,username,status,creator_uid,bot_token,auto_approve,kind,management_scope,
		 management_space_id,created_by,publication_state,lifecycle_pending)
		VALUES (?,?,1,'',?,1,'avatar','space',?,'manager','published',0)`, robotID, robotID, token, spaceID).Exec()
	require.NoError(t, err)
}

func avatarRequest(t *testing.T, handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func requireAvatarUnsupported(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	require.Equal(t, errcode.ErrBotAPIAvatarUnsupported.ID, body.Error.Code, w.Body.String())
}

func TestAvatarActualForbiddenRoutesAreDeniedAtAuthBoundary(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	robotID, token, spaceID := "avatar_policy_"+util.GenerUUID()[:8], "bf_avatar_policy_"+util.GenerUUID()[:8], "space_policy_"+util.GenerUUID()[:8]
	seedActiveAvatarOnContext(t, ctx.DB(), robotID, token, spaceID)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/bot/createGroup"},
		{http.MethodPut, "/v1/bot/groups/g/info"},
		{http.MethodPost, "/v1/bot/groups/g/members/add"},
		{http.MethodPost, "/v1/bot/groups/g/members/remove"},
		// These are separate Bot-token route groups rather than descendants of
		// botAPI. Keep them in this matrix so a future route refactor cannot
		// accidentally let an Avatar manage a group through a side surface.
		{http.MethodPost, "/v1/bot/groups/g/incoming-webhooks"},
		{http.MethodPost, "/v1/bot/groups/g/threads/t/incoming-webhooks"},
		{http.MethodPost, "/v1/bot/groups/g/threads"},
		{http.MethodDelete, "/v1/bot/groups/g/threads/t"},
		{http.MethodPut, "/v1/bot/groups/g/md"},
		{http.MethodPut, "/v1/bot/groups/g/threads/t/md"},
		{http.MethodPost, "/v1/bot/users/batch"},
		{http.MethodGet, "/v1/bot/space/members"},
		{http.MethodPost, "/v1/bot/messages/_search"},
		{http.MethodPost, "/v1/bot/voice/transcribe"},
		{http.MethodGet, "/v1/bot/obo-grant"},
		{http.MethodGet, "/v1/bot/space/principals/u?space_id=" + spaceID},
		{http.MethodGet, "/v1/bot/resolve/targets?name=x"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			requireAvatarUnsupported(t, avatarRequest(t, s.GetRoute(), tc.method, tc.path, token))
		})
	}
}

func TestAvatarUnknownMountedRouteDefaultsToDeny(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	robotID, token, spaceID := "avatar_unknown_"+util.GenerUUID()[:8], "bf_avatar_unknown_"+util.GenerUUID()[:8], "space_unknown_"+util.GenerUUID()[:8]
	seedActiveAvatarOnContext(t, ctx.DB(), robotID, token, spaceID)

	ba := &BotAPI{ctx: ctx, db: newBotAPIDB(ctx), Log: log.NewTLog("avatar-policy-test")}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	reached := false
	r.GET("/v1/bot/future-capability", ba.authBot(), func(c *wkhttp.Context) {
		reached = true
		c.ResponseOK()
	})
	requireAvatarUnsupported(t, avatarRequest(t, r, http.MethodGet, "/v1/bot/future-capability", token))
	require.False(t, reached)
}

func TestAvatarRegisterCanonicalTypeAndLifecycleGate(t *testing.T) {
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/user/token", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":200}`))
	}))
	defer im.Close()

	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	ctx.GetConfig().WuKongIM.APIURL = im.URL
	robotID, token, spaceID := "avatar_register_"+util.GenerUUID()[:8], "bf_avatar_register_"+util.GenerUUID()[:8], "space_register_"+util.GenerUUID()[:8]
	seedActiveAvatarOnContext(t, ctx.DB(), robotID, token, spaceID)

	postRegister := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/bot/register", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	ok := postRegister()
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	var response struct {
		RobotID  string `json:"robot_id"`
		OwnerUID string `json:"owner_uid"`
		BotType  string `json:"bot_type"`
	}
	require.NoError(t, json.Unmarshal(ok.Body.Bytes(), &response))
	require.Equal(t, robotID, response.RobotID)
	require.Empty(t, response.OwnerUID)
	require.Equal(t, "avatar", response.BotType)

	_, err := ctx.DB().Update("robot").Set("lifecycle_pending", 1).Where("robot_id=?", robotID).Exec()
	require.NoError(t, err)
	denied := postRegister()
	require.NotEqual(t, http.StatusOK, denied.Code, denied.Body.String())

	_, err = ctx.DB().Update("robot").SetMap(map[string]interface{}{
		"kind": "user", "status": 1, "creator_uid": "", "publication_state": "draft", "lifecycle_pending": 0,
	}).Where("robot_id=?", robotID).Exec()
	require.NoError(t, err)
	ownerless := postRegister()
	require.NotEqual(t, http.StatusOK, ownerless.Code, ownerless.Body.String())
}

func TestAvatarResourceAuthorizationCoversDMGroupThreadAndProject(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	robotID, token, spaceID := "avatar_resource_"+util.GenerUUID()[:8], "bf_avatar_resource_"+util.GenerUUID()[:8], "space_resource_"+util.GenerUUID()[:8]
	seedActiveAvatarOnContext(t, ctx.DB(), robotID, token, spaceID)

	humanID, outsiderID := "human_"+util.GenerUUID()[:8], "outsider_"+util.GenerUUID()[:8]
	for _, uid := range []string{humanID, outsiderID} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO `user` (uid,name,username,short_no,status,robot,is_destroy) VALUES (?,?,?,?,1,0,0)",
			uid, uid, uid, util.GenerUUID()[:8]).Exec()
		require.NoError(t, err)
	}
	_, err := ctx.DB().InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,0,1)", spaceID, humanID).Exec()
	require.NoError(t, err)
	otherSpace := "other_" + util.GenerUUID()[:8]
	_, err = ctx.DB().InsertBySql("INSERT INTO space (space_id,name,status) VALUES (?,?,1)", otherSpace, otherSpace).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,0,1)", otherSpace, outsiderID).Exec()
	require.NoError(t, err)
	// A stale friendship must not broaden DM authority beyond live shared Space.
	_, err = ctx.DB().InsertBySql("INSERT INTO friend (uid,to_uid,is_deleted) VALUES (?,?,0),(?,?,0)", robotID, outsiderID, outsiderID, robotID).Exec()
	require.NoError(t, err)

	allowed, err := botpolicy.CanAccessChannel(ctx.DB(), robotID, humanID, common.ChannelTypePerson.Uint8(), spaceID)
	require.NoError(t, err)
	require.True(t, allowed)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, outsiderID, common.ChannelTypePerson.Uint8(), "")
	require.NoError(t, err)
	require.False(t, allowed)

	ba := &BotAPI{ctx: ctx, db: newBotAPIDB(ctx), Log: log.NewTLog("avatar-event-policy-test")}
	visible := ba.filterAppBotEvents(BotKindAvatar, robotID, []*eventResp{
		{EventID: 1, Message: &messageResp{ChannelID: "synthetic-person-channel", ChannelType: common.ChannelTypePerson.Uint8(), FromUID: humanID}},
		{EventID: 2, Message: &messageResp{ChannelID: "synthetic-stale-channel", ChannelType: common.ChannelTypePerson.Uint8(), FromUID: outsiderID}},
	})
	require.Len(t, visible, 1)
	require.Equal(t, int64(1), visible[0].EventID, "live shared-Space DM survives while stale friendship is filtered")

	// Platform Avatars can hold many Space seats. DM authorization must return
	// the Space actually shared with this peer, not the Avatar's oldest seat,
	// because that value is injected into the outbound message envelope.
	_, err = ctx.DB().Update("robot").SetMap(map[string]interface{}{
		"management_scope": "platform", "management_space_id": "",
	}).Where("robot_id=?", robotID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql("INSERT INTO space_member (space_id,uid,role,status) VALUES (?,?,0,1)", otherSpace, robotID).Exec()
	require.NoError(t, err)
	sharedSpace, err := botpolicy.ResolveAvatarDMSharedSpace(ctx.DB(), robotID, outsiderID, "")
	require.NoError(t, err)
	require.Equal(t, otherSpace, sharedSpace)
	sharedSpace, err = botpolicy.ResolveAvatarDMSharedSpace(ctx.DB(), robotID, outsiderID, spaceID)
	require.NoError(t, err)
	require.Empty(t, sharedSpace, "a caller-provided Space hint cannot select a non-shared seat")

	groupNo := "avatar_group_" + util.GenerUUID()[:8]
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no,name,creator,status,version,space_id,project_id) VALUES (?,?,?,1,1,?,'')",
		groupNo, "Avatar group", humanID, spaceID).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(`INSERT INTO group_member
		(group_no,uid,role,version,is_deleted,status,vercode,robot,invite_uid,is_external)
		VALUES (?,?,0,1,0,1,?,1,?,0)`, groupNo, robotID, util.GenerUUID(), humanID).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed, "an old ordinary-group row must not grant an Avatar access")
	_, err = ctx.DB().Update("group").Set("purpose", aiteampkg.GroupPurpose).Where("group_no=?", groupNo).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(`INSERT INTO ai_team_agent
		(space_id,user_uid,bot_id,group_no,is_added,container_state) VALUES (?,?,?,?,1,2)`,
		spaceID, humanID, robotID, groupNo).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.True(t, allowed, "the server-owned AI Team container grants access")

	shortID := "thread_" + util.GenerUUID()[:8]
	_, err = ctx.DB().InsertBySql("INSERT INTO thread (short_id,group_no,name,creator_uid,status) VALUES (?,?,?,?,1)",
		shortID, groupNo, "Avatar thread", humanID).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo+"____"+shortID,
		common.ChannelTypeCommunityTopic.Uint8(), spaceID)
	require.NoError(t, err)
	require.True(t, allowed)

	projectID := "project_" + util.GenerUUID()[:8]
	_, err = ctx.DB().Update("group").SetMap(map[string]interface{}{"project_id": projectID, "purpose": ""}).Where("group_no=?", groupNo).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed, "a Space seat alone must not open a project group")
	_, err = ctx.DB().InsertBySql(`INSERT INTO octo_project_member
		(project_id,uid,space_id,role,status,removing,invite_uid,created_at,updated_at)
		VALUES (?,?,?,0,1,0,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		projectID, robotID, spaceID, humanID).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed, "a project seat cannot reopen ordinary-group access for an Avatar")

	_, err = ctx.DB().Update("group_member").Set("is_deleted", 1).Where("group_no=? AND uid=?", groupNo, robotID).Exec()
	require.NoError(t, err)
	allowed, err = botpolicy.CanAccessChannel(ctx.DB(), robotID, groupNo, common.ChannelTypeGroup.Uint8(), spaceID)
	require.NoError(t, err)
	require.False(t, allowed)
}
