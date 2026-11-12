package bot_api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/stretchr/testify/require"
)

const (
	cliGroupTestToken         = "cli-group-test-token"
	cliGroupTestServiceSecret = "cli-group-test-service-secret-with-at-least-32-bytes"
)

// cliGroupAuthFixture speaks the Auth SDK's HTTP contract; requests still pass
// through NewRouter and the real SDK rather than injecting a trusted principal.
func cliGroupAuthFixture(t *testing.T, ownerID, ownerName, spaceID, botID, botName string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/internal/tokens/_resolve" {
			t.Errorf("unexpected Auth SDK request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+cliGroupTestServiceSecret {
			t.Errorf("Auth SDK Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Auth SDK request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if request.Token != cliGroupTestToken {
			t.Errorf("Auth SDK token = %q", request.Token)
			http.Error(w, "unexpected token", http.StatusBadRequest)
			return
		}

		bindings := make([]map[string]string, 0, 1)
		if botID != "" {
			bindings = append(bindings, map[string]string{"bot_id": botID, "bot_name": botName})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"principal": map[string]string{
					"token_id":     "cli-group-test-id",
					"space_id":     spaceID,
					"owner_id":     ownerID,
					"owner_name":   ownerName,
					"subject_type": "user",
					"subject_id":   ownerID,
				},
				"permissions": []map[string]interface{}{
					{"domain": "im.groups", "actions": []string{"im.groups:write", "im.groups:read"}},
					{"domain": "im.threads", "actions": []string{"im.threads:write", "im.threads:read"}},
				},
				"bot_bindings": bindings,
			},
		})
	}))
}

func newCLIGroupTestRouter(t *testing.T, source *wkhttp.WKHttp, ownerID, ownerName, spaceID, botID, botName string) (*wkhttp.WKHttp, func()) {
	t.Helper()
	authServer := cliGroupAuthFixture(t, ownerID, ownerName, spaceID, botID, botName)
	router, err := cliv2.NewRouter(authServer.URL, cliGroupTestServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(router, func(c *wkhttp.Context) {
		if c.GetLoginUID() == "" {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, router))
	return router, authServer.Close
}

func newCLIGroupTestSource(t *testing.T) (*wkhttp.WKHttp, *config.Context) {
	t.Setenv("DM_THREAD_ON", "true")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	return s.GetRoute(), ctx
}

func cliGroupRequest(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cliGroupTestToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func requireCLIPageResponse(t *testing.T, response *httptest.ResponseRecorder, total, page, pageSize int64, dataLen int) {
	t.Helper()
	var got struct {
		Data       []json.RawMessage `json:"data"`
		Pagination struct {
			Total    int64 `json:"total"`
			Page     int64 `json:"page"`
			PageSize int64 `json:"page_size"`
		} `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.Equal(t, total, got.Pagination.Total)
	require.Equal(t, page, got.Pagination.Page)
	require.Equal(t, pageSize, got.Pagination.PageSize)
	require.Len(t, got.Data, dataLen)
}

func seedCLIGroupUser(t *testing.T, ctx *config.Context, uid, name string, robot int) {
	t.Helper()
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{
		UID:     uid,
		Name:    name,
		ShortNo: "cli_" + uid,
		Robot:   robot,
	}))
}

func seedCLIGroupSpace(t *testing.T, ctx *config.Context, spaceID, creator string, memberUIDs ...string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("space").Columns("space_id", "name", "creator", "status").
		Values(spaceID, "CLI test Space", creator, 1).Exec()
	require.NoError(t, err)
	for _, uid := range memberUIDs {
		_, err = ctx.DB().InsertInto("space_member").Columns("space_id", "uid", "role", "status").
			Values(spaceID, uid, 0, 1).Exec()
		require.NoError(t, err)
	}
}

func seedCLIGroup(t *testing.T, ctx *config.Context, groupNo, spaceID, ownerID string, memberUIDs ...string) {
	t.Helper()
	db := group.NewDB(ctx)
	require.NoError(t, db.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "CLI test group",
		Creator: ownerID,
		Status:  group.GroupStatusNormal,
		Version: 1,
		SpaceID: spaceID,
	}))
	members := append([]string{ownerID}, memberUIDs...)
	for _, uid := range members {
		role := group.MemberRoleCommon
		if uid == ownerID {
			role = group.MemberRoleCreator
		}
		require.NoError(t, db.InsertMember(&group.MemberModel{
			GroupNo: groupNo,
			UID:     uid,
			Role:    role,
			Status:  int(common.GroupMemberStatusNormal),
			Version: 1,
			Vercode: util.GenerUUID(),
		}))
	}
}

// seedCLIGroupUserWithMsgExpire seeds a user together with the personal
// message expiry that the legacy group creation path copies to channel_setting.
func seedCLIGroupUserWithMsgExpire(t *testing.T, ctx *config.Context, uid, name string, robot int, msgExpireSecond int64) {
	t.Helper()
	require.NoError(t, user.NewDB(ctx).Insert(&user.Model{
		UID:             uid,
		Name:            name,
		ShortNo:         "cli_" + uid,
		Robot:           robot,
		MsgExpireSecond: msgExpireSecond,
	}))
}

// setCLIGroupMemberFlags rewrites the bot columns that seedCLIGroup leaves at
// their zero values; bot_admin drives the Bot/v1-administrator parity checks.
func setCLIGroupMemberFlags(t *testing.T, ctx *config.Context, groupNo, uid string, botAdmin, robot int) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET bot_admin=?, robot=? WHERE group_no=? AND uid=?",
		botAdmin, robot, groupNo, uid,
	).Exec()
	require.NoError(t, err)
}

func seedCLIGroupAppConfig(t *testing.T, ctx *config.Context, inviteSystemAccountJoinGroupOn int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, ?)",
		inviteSystemAccountJoinGroupOn,
	).Exec()
	require.NoError(t, err)
}

func TestCLIGroupCreateKeepsTokenOwnerAndBoundSpace(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_group_owner"
	const memberID = "cli_group_member"
	const botID = "cli_group_bot"
	const targetBotID = "cli_group_target_bot"
	const spaceID = "cli_group_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, memberID, "Member", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, targetBotID, "Target Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, memberID, botID, targetBotID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, "/v2/cli/groups",
		`{"name":"CLI created","members":["cli_group_member","cli_group_target_bot"],"creator":"untrusted_creator"}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var created struct {
		GroupNo string `db:"group_no"`
		Creator string `db:"creator"`
		SpaceID string `db:"space_id"`
	}
	_, err := ctx.DB().Select("group_no", "creator", "space_id").From("`group`").
		Where("name=?", "CLI created").Load(&created)
	require.NoError(t, err)
	require.NotEmpty(t, created.GroupNo)
	require.Equal(t, ownerID, created.Creator)
	require.Equal(t, spaceID, created.SpaceID)

	var botMembers int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", created.GroupNo, botID).LoadOne(&botMembers)
	require.NoError(t, err)
	require.Equal(t, 1, botMembers)
	var targetBotMembers int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", created.GroupNo, targetBotID).LoadOne(&targetBotMembers)
	require.NoError(t, err)
	require.Zero(t, targetBotMembers)
}
func TestCLIGroupCreateRequiresHumanMemberForBoundBot(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_bot_create_owner"
	const botID = "cli_bot_create_actor"
	const targetBotID = "cli_bot_create_target"
	const spaceID = "cli_bot_create_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, targetBotID, "Target Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, targetBotID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, "/v2/cli/groups",
		`{"name":"Bots only","members":["cli_bot_create_target"]}`)
	require.Equalf(t, http.StatusBadRequest, response.Code, response.Body.String())

	var groupCount int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM `group` WHERE name=?", "Bots only").LoadOne(&groupCount)
	require.NoError(t, err)
	require.Zero(t, groupCount)
}

func TestCLIGroupMemberAddFiltersBotsForBoundBot(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_bot_add_owner"
	const botID = "cli_bot_add_actor"
	const humanID = "cli_bot_add_human"
	const targetBotID = "cli_bot_add_target"
	const spaceID = "cli_bot_add_space"
	const groupNo = "g_cli_bot_add"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupUser(t, ctx, targetBotID, "Target Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanID, targetBotID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	path := fmt.Sprintf("/v2/cli/groups/%s/members/_add", groupNo)
	response := cliGroupRequest(t, router, http.MethodPost, path,
		`{"members":["cli_bot_add_human","cli_bot_add_target"]}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
	var got struct {
		Data struct {
			Added       int      `json:"added"`
			SkippedBots []string `json:"skipped_bots"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.Equal(t, 1, got.Data.Added)
	require.Equal(t, []string{targetBotID}, got.Data.SkippedBots)

	var humanCount, botCount int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, humanID).LoadOne(&humanCount)
	require.NoError(t, err)
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, targetBotID).LoadOne(&botCount)
	require.NoError(t, err)
	require.Equal(t, 1, humanCount)
	require.Zero(t, botCount)

	response = cliGroupRequest(t, router, http.MethodPost, path, `{"members":["cli_bot_add_target"]}`)
	require.Equalf(t, http.StatusBadRequest, response.Code, response.Body.String())
}

func TestCLIGroupMemberAddRejectsAIContainer(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_ai_add_owner"
	const botID = "cli_ai_add_actor"
	const humanID = "cli_ai_add_human"
	const spaceID = "cli_ai_add_space"
	const groupNo = "g_cli_ai_add"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	_, err := ctx.DB().Update("group").Set("purpose", aiteam.GroupPurpose).Where("group_no=?", groupNo).Exec()
	require.NoError(t, err)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost,
		fmt.Sprintf("/v2/cli/groups/%s/members/_add", groupNo), `{"members":["cli_ai_add_human"]}`)
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())

	var count int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0", groupNo, humanID).LoadOne(&count)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestCLIThreadCreateRejectsAIContainer(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_ai_thread_owner"
	const botID = "cli_ai_thread_actor"
	const spaceID = "cli_ai_thread_space"
	const groupNo = "g_cli_ai_thread"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	_, err := ctx.DB().Update("group").Set("purpose", aiteam.GroupPurpose).Where("group_no=?", groupNo).Exec()
	require.NoError(t, err)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost,
		fmt.Sprintf("/v2/cli/groups/%s/threads", groupNo), `{"name":"Should be blocked"}`)
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())

	var count int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM thread WHERE group_no=? AND name=?", groupNo, "Should be blocked").LoadOne(&count)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestCLIGroupAndThreadListsUsePagePagination(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_page_owner"
	const outsiderID = "cli_page_outsider"
	const spaceID = "cli_page_space"
	const groupNo = "g_cli_page"
	const hiddenGroupNo = "g_cli_page_hidden"
	const outsideSpaceID = "cli_page_outside_space"
	const outsideGroupNo = "g_cli_page_outside"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, outsiderID, "Outsider", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, outsiderID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)
	seedCLIGroup(t, ctx, hiddenGroupNo, spaceID, outsiderID)
	seedCLIGroupSpace(t, ctx, outsideSpaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, outsideGroupNo, outsideSpaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()

	checkPage := func(path string, total, page, pageSize int64, dataLen int) {
		t.Helper()
		response := cliGroupRequest(t, router, http.MethodGet, path, "")
		require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
		requireCLIPageResponse(t, response, total, page, pageSize, dataLen)
	}
	checkPage("/v2/cli/groups", 1, 1, 20, 1)
	checkPage("/v2/cli/groups/"+groupNo+"/members", 1, 1, 20, 1)

	createThread := cliGroupRequest(t, router, http.MethodPost,
		fmt.Sprintf("/v2/cli/groups/%s/threads", groupNo), `{"name":"Pagination topic"}`)
	require.Equalf(t, http.StatusOK, createThread.Code, createThread.Body.String())
	var threadID string
	err := ctx.DB().Select("short_id").From("thread").Where("group_no=? AND name=?", groupNo, "Pagination topic").LoadOne(&threadID)
	require.NoError(t, err)

	threadsPath := "/v2/cli/groups/" + groupNo + "/threads"
	checkPage(threadsPath, 1, 1, 20, 1)
	checkPage(threadsPath+"?page=2&page_size=1", 1, 2, 1, 0)
	checkPage("/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/members", 1, 1, 20, 1)
	checkPage("/v2/cli/groups/"+groupNo+"/members?page_size=100", 1, 1, 100, 1)
	checkPage(threadsPath+"?page_size=100", 1, 1, 100, 1)
	checkPage("/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/members?page_size=100", 1, 1, 100, 1)
	checkPage("/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/members?page=2&page_size=1", 1, 2, 1, 0)
	checkPage("/v2/cli/groups?page_size=100", 1, 1, 100, 1)

	tooLargeGroupPage := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo+"/members?page_size=101", "")
	require.Equalf(t, http.StatusBadRequest, tooLargeGroupPage.Code, tooLargeGroupPage.Body.String())
	tooLargeGroupListPage := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups?page_size=101", "")
	require.Equalf(t, http.StatusBadRequest, tooLargeGroupListPage.Code, tooLargeGroupListPage.Body.String())
	tooLargeThreadPage := cliGroupRequest(t, router, http.MethodGet, threadsPath+"?page_size=101", "")
	require.Equalf(t, http.StatusBadRequest, tooLargeThreadPage.Code, tooLargeThreadPage.Body.String())
	tooLargeThreadMemberPage := cliGroupRequest(t, router, http.MethodGet,
		"/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/members?page_size=101", "")
	require.Equalf(t, http.StatusBadRequest, tooLargeThreadMemberPage.Code, tooLargeThreadMemberPage.Body.String())
}

// TestCLIThreadCreateUsesBoundBotAsCreatorWithoutOBO also sends the legacy
// OBO/creator impersonation fields a caller might use to attribute the thread
// to the Token owner: the v2 route ignores them and keeps the bound Bot as the
// creator of the thread it creates.
func TestCLIThreadCreateUsesBoundBotAsCreatorWithoutOBO(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_owner"
	const botID = "cli_thread_bot"
	const spaceID = "cli_thread_space"
	const groupNo = "1234567890abcdef1234567890abcdef"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, fmt.Sprintf("/v2/cli/groups/%s/threads", groupNo),
		`{"name":"Bot-owned topic","creator":"cli_thread_owner","creator_uid":"cli_thread_owner","on_behalf_of":"cli_thread_owner"}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var creatorUID string
	err := ctx.DB().Select("creator_uid").From("thread").Where("group_no=? AND name=?", groupNo, "Bot-owned topic").LoadOne(&creatorUID)
	require.NoError(t, err)
	require.Equal(t, botID, creatorUID)
}

func TestCLIThreadCreateWithoutBotUsesTokenOwner(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_user"
	const spaceID = "cli_thread_user_space"
	const groupNo = "abcdef1234567890abcdef1234567890"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, fmt.Sprintf("/v2/cli/groups/%s/threads", groupNo),
		`{"name":"User-owned topic"}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var creatorUID string
	err := ctx.DB().Select("creator_uid").From("thread").Where("group_no=? AND name=?", groupNo, "User-owned topic").LoadOne(&creatorUID)
	require.NoError(t, err)
	require.Equal(t, ownerID, creatorUID)
}

func TestCLIGroupReadUsesTokenOwnerWithoutBot(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_read_owner"
	const groupNo = "fedcba0987654321fedcba0987654321"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupSpace(t, ctx, "cli_read_space", ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, "cli_read_space", ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", "cli_read_space", "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), groupNo)
}

func TestCLIGroupReadAcceptsOpaqueLegacyGroupID(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_legacy_group_owner"
	const groupNo = "g_cli_legacy"
	const spaceID = "cli_legacy_group_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), groupNo)
}

func TestCLIGroupReadRequiresBoundBotSpaceMembership(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_bot_space_owner"
	const botID = "cli_unadmitted_bot"
	const spaceID = "cli_bot_space_membership"
	const groupNo = "g_cli_bot_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestCLIGroupReadRequiresBoundBotMembership(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_bot_group_owner"
	const botID = "cli_group_actor_bot"
	const spaceID = "cli_bot_group_space"
	const groupNo = "g_cli_bot_actor"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestCLIGroupReadRequiresTokenOwnerBoundSpaceMembership(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_space_owner_not_member"
	const botID = "cli_space_member_bot"
	const spaceID = "cli_owner_membership_space"
	const groupNo = "g_cli_space_member"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestCLIGroupReadRejectsGroupOutsideTokenSpace(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_scope_owner"
	const groupNo = "00112233445566778899aabbccddeeff"
	const tokenSpaceID = "cli_token_space"
	const groupSpaceID = "cli_other_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupSpace(t, ctx, tokenSpaceID, ownerID, ownerID)
	seedCLIGroupSpace(t, ctx, groupSpaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, groupSpaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", tokenSpaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, response.Code, response.Body.String())
}

func TestCLIThreadCreateRejectsParentOutsideTokenSpace(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_scope_owner"
	const botID = "cli_thread_scope_bot"
	const tokenSpaceID = "cli_thread_token_space"
	const groupSpaceID = "cli_thread_other_space"
	const groupNo = "ffeeddccbbaa99887766554433221100"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, tokenSpaceID, ownerID, ownerID, botID)
	seedCLIGroupSpace(t, ctx, groupSpaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, groupSpaceID, ownerID, botID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", tokenSpaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, fmt.Sprintf("/v2/cli/groups/%s/threads", groupNo),
		`{"name":"Out-of-Space topic"}`)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, response.Code, response.Body.String())

	var count int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM thread WHERE group_no=? AND name=?", groupNo, "Out-of-Space topic").LoadOne(&count)
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestCLIGroupUpdateMarkdownAllowsBoundBotAdmin pins the v1 bot contract onto
// the v2 route: a bound Bot that is a group member with bot_admin=1 may write
// GROUP.md even though it is not the creator or a human manager. The response
// version and stored author must both come from that Bot.
func TestCLIGroupUpdateMarkdownAllowsBoundBotAdmin(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_md_admin_owner"
	const botID = "cli_md_admin_bot"
	const memberID = "cli_md_admin_member"
	const spaceID = "cli_md_admin_space"
	const groupNo = "g_cli_md_admin"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, memberID, "Member", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, memberID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID, memberID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 1, 1)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPut, "/v2/cli/groups/"+groupNo+"/md",
		`{"content":"# bot admin markdown"}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var got struct {
		Data struct {
			Version int64 `json:"version"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.EqualValues(t, 1, got.Data.Version)

	var stored struct {
		Content   string `db:"content"`
		UpdatedBy string `db:"updated_by"`
	}
	_, err := ctx.DB().SelectBySql(
		"SELECT IFNULL(group_md,'') AS content, group_md_updated_by AS updated_by FROM `group` WHERE group_no=?",
		groupNo,
	).Load(&stored)
	require.NoError(t, err)
	require.Equal(t, "# bot admin markdown", stored.Content)
	require.Equal(t, botID, stored.UpdatedBy)
}

// TestCLIGroupUpdateMarkdownRejectsBotWithoutAdmin is the negative side of the
// same gate: membership and the bound Bot identity alone must not grant
// GROUP.md write access.
func TestCLIGroupUpdateMarkdownRejectsBotWithoutAdmin(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_md_plain_owner"
	const botID = "cli_md_plain_bot"
	const spaceID = "cli_md_plain_space"
	const groupNo = "g_cli_md_plain"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 0, 1)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPut, "/v2/cli/groups/"+groupNo+"/md",
		`{"content":"# must not be written"}`)
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"FORBIDDEN"`)

	var stored struct {
		Content string `db:"content"`
	}
	_, err := ctx.DB().SelectBySql("SELECT IFNULL(group_md,'') AS content FROM `group` WHERE group_no=?", groupNo).Load(&stored)
	require.NoError(t, err)
	require.Empty(t, stored.Content)
}

// TestCLIGroupMemberRemoveAllowsBotAdminToRemoveHumanMember covers the positive
// removal contract: bot_admin is equivalent to a human manager for removing a
// compliant human common-role member, and the removal is persisted.
func TestCLIGroupMemberRemoveAllowsBotAdminToRemoveHumanMember(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_rm_admin_owner"
	const botID = "cli_rm_admin_bot"
	const humanID = "cli_rm_admin_human"
	const spaceID = "cli_rm_admin_space"
	const groupNo = "g_cli_rm_admin"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID, humanID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 1, 1)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/members/_remove", `{"members":["cli_rm_admin_human"]}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var got struct {
		Data struct {
			Removed int `json:"removed"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.Equal(t, 1, got.Data.Removed)

	var isDeleted int
	err := ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, humanID,
	).LoadOne(&isDeleted)
	require.NoError(t, err)
	require.Equal(t, 1, isDeleted)
}

// TestCLIGroupMemberRemoveRejectsBotAdminRemovingPrivilegedMembers keeps the
// v1 guard: bot_admin cannot kick the creator or a human manager, and a mixed
// batch must be rejected as a whole rather than silently dropping the
// privileged target.
func TestCLIGroupMemberRemoveRejectsBotAdminRemovingPrivilegedMembers(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_rm_priv_owner"
	const botID = "cli_rm_priv_bot"
	const managerID = "cli_rm_priv_manager"
	const humanID = "cli_rm_priv_human"
	const spaceID = "cli_rm_priv_space"
	const groupNo = "g_cli_rm_priv"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, managerID, "Manager", 0)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, managerID, humanID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID, managerID, humanID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 1, 1)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET role=? WHERE group_no=? AND uid=?",
		group.MemberRoleManager, groupNo, managerID,
	).Exec()
	require.NoError(t, err)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()

	mixed := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/members/_remove",
		`{"members":["cli_rm_priv_manager","cli_rm_priv_human"]}`)
	require.Equalf(t, http.StatusForbidden, mixed.Code, mixed.Body.String())
	require.Contains(t, mixed.Body.String(), `"FORBIDDEN"`)

	creator := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/members/_remove", `{"members":["cli_rm_priv_owner"]}`)
	require.Equalf(t, http.StatusForbidden, creator.Code, creator.Body.String())

	for _, uid := range []string{ownerID, managerID, humanID} {
		var isDeleted int
		queryErr := ctx.DB().SelectBySql(
			"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, uid,
		).LoadOne(&isDeleted)
		require.NoError(t, queryErr)
		require.Zero(t, isDeleted, "uid %s must remain an active member", uid)
	}
}

// TestCLIGroupMemberRemoveRejectsBotWithoutAdmin verifies the negative side:
// plain bot membership does not include member-removal authority.
func TestCLIGroupMemberRemoveRejectsBotWithoutAdmin(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_rm_plain_owner"
	const botID = "cli_rm_plain_bot"
	const humanID = "cli_rm_plain_human"
	const spaceID = "cli_rm_plain_space"
	const groupNo = "g_cli_rm_plain"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID, humanID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 0, 1)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/members/_remove", `{"members":["cli_rm_plain_human"]}`)
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())

	var isDeleted int
	err := ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, humanID,
	).LoadOne(&isDeleted)
	require.NoError(t, err)
	require.Zero(t, isDeleted)
}

// TestCLIGroupMemberAddWithInviteKeepsLegacyBotBehavior fixes the intent of the
// invite-confirmation switch: it is a human invitation rule. The v1 Bot endpoint
// admits human members with a plain bot membership, so the v2 adapter must not
// start requiring creator/manager (and must not reject the bound Bot) just
// because invite=1.
func TestCLIGroupMemberAddWithInviteKeepsLegacyBotBehavior(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_invite_owner"
	const botID = "cli_invite_bot"
	const targetID = "cli_invite_target"
	const spaceID = "cli_invite_space"
	const groupNo = "g_cli_invite"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, targetID, "Target", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, targetID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 0, 1)
	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET invite=1 WHERE group_no=?", groupNo).Exec()
	require.NoError(t, err)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/members/_add", `{"members":["cli_invite_target"]}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var got struct {
		Data struct {
			Added int `json:"added"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.Equal(t, 1, got.Data.Added)

	var isDeleted int
	err = ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no=? AND uid=?", groupNo, targetID,
	).LoadOne(&isDeleted)
	require.NoError(t, err)
	require.Zero(t, isDeleted)
}

// TestCLIGroupCreateUserSubjectEnforcesDailyCreateLimit preserves the legacy
// user-subject cap: with the creator already at the same-day group limit, one
// more user-subject creation is rejected with the v2 rate-limit envelope and
// no row is written. The limit is read from the same configuration default the
// legacy handler uses.
func TestCLIGroupCreateUserSubjectEnforcesDailyCreateLimit(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_daily_owner"
	const memberID = "cli_daily_member"
	const spaceID = "cli_daily_space"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, memberID, "Member", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, memberID)

	limit := ctx.GetConfig().Group.SameDayCreateMaxCount
	require.Greater(t, limit, 0)
	for i := range limit {
		seedCLIGroup(t, ctx, fmt.Sprintf("g_cli_daily_seed_%d", i), spaceID, ownerID)
	}

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, "/v2/cli/groups",
		`{"name":"Daily blocked","members":["cli_daily_member"]}`)
	require.Equalf(t, http.StatusTooManyRequests, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"RATE_LIMITED"`)

	var createdCount int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM `group` WHERE name=?", "Daily blocked").LoadOne(&createdCount)
	require.NoError(t, err)
	require.Zero(t, createdCount)
}

// TestCLIGroupCreateUserSubjectRejectsFileHelperWhenInviteDisabled preserves the
// legacy system-account admission gate for user-subject creation: with the
// application setting off, a request carrying the file-helper UID is refused
// and no group row is created.
func TestCLIGroupCreateUserSubjectRejectsFileHelperWhenInviteDisabled(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_fh_owner"
	const humanID = "cli_fh_human"
	const spaceID = "cli_fh_space"
	fileHelperID := ctx.GetConfig().Account.FileHelperUID
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupUser(t, ctx, fileHelperID, "File Helper", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, humanID)
	seedCLIGroupAppConfig(t, ctx, 0)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, "/v2/cli/groups",
		fmt.Sprintf(`{"name":"File helper group","members":["%s","%s"]}`, fileHelperID, humanID))
	require.Equalf(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"VALIDATION_ERROR"`)

	var createdCount int
	err := ctx.DB().SelectBySql("SELECT COUNT(*) FROM `group` WHERE name=?", "File helper group").LoadOne(&createdCount)
	require.NoError(t, err)
	require.Zero(t, createdCount)
}

// TestCLIGroupCreateUserSubjectAppliesMsgAutoDelete preserves the legacy
// user-subject message-expiry setting: the creator's msg_expire_second is
// copied onto the new group channel.
func TestCLIGroupCreateUserSubjectAppliesMsgAutoDelete(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_expire_owner"
	const memberID = "cli_expire_member"
	const spaceID = "cli_expire_space"
	const msgExpireSecond int64 = 3600
	seedCLIGroupUserWithMsgExpire(t, ctx, ownerID, "Owner", 0, msgExpireSecond)
	seedCLIGroupUser(t, ctx, memberID, "Member", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, memberID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPost, "/v2/cli/groups",
		`{"name":"Expiring group","members":["cli_expire_member"]}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var created struct {
		Data struct {
			GroupID string `json:"group_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.NotEmpty(t, created.Data.GroupID)

	var msgAutoDelete int64
	err := ctx.DB().SelectBySql(
		"SELECT msg_auto_delete FROM channel_setting WHERE channel_id=? AND channel_type=?",
		created.Data.GroupID, common.ChannelTypeGroup.Uint8(),
	).LoadOne(&msgAutoDelete)
	require.NoError(t, err)
	require.Equal(t, msgExpireSecond, msgAutoDelete)
}

// TestCLIGroupListReportsRoleMemberCountAndMarkdownPermission asserts that the
// list projection is computed from the actor's real membership row instead of
// left at zero values: role, member_count (deleted rows excluded), and
// can_edit_group_md (creator/manager or bot_admin) must match the database.
func TestCLIGroupListReportsRoleMemberCountAndMarkdownPermission(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_list_owner"
	const botID = "cli_list_bot"
	const humanAID = "cli_list_human_a"
	const humanBID = "cli_list_human_b"
	const spaceID = "cli_list_space"
	const managerGroupNo = "g_cli_list_manager"
	const adminGroupNo = "g_cli_list_admin"
	const commonGroupNo = "g_cli_list_common"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanAID, "Human A", 0)
	seedCLIGroupUser(t, ctx, humanBID, "Human B", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanAID, humanBID)

	seedCLIGroup(t, ctx, managerGroupNo, spaceID, ownerID, botID, humanAID)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET role=? WHERE group_no=? AND uid=?",
		group.MemberRoleManager, managerGroupNo, botID,
	).Exec()
	require.NoError(t, err)
	setCLIGroupMemberFlags(t, ctx, managerGroupNo, botID, 1, 1)
	require.NoError(t, group.NewDB(ctx).InsertMember(&group.MemberModel{
		GroupNo:   managerGroupNo,
		UID:       humanBID,
		Role:      group.MemberRoleCommon,
		Status:    int(common.GroupMemberStatusNormal),
		IsDeleted: 1,
		Version:   1,
		Vercode:   util.GenerUUID(),
	}))

	seedCLIGroup(t, ctx, adminGroupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, adminGroupNo, botID, 1, 1)

	seedCLIGroup(t, ctx, commonGroupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, commonGroupNo, botID, 0, 1)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups?page_size=100", "")
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
	requireCLIPageResponse(t, response, 3, 1, 100, 3)

	var got struct {
		Data []struct {
			GroupNo        string `json:"group_no"`
			Role           int    `json:"role"`
			MemberCount    int    `json:"member_count"`
			CanEditGroupMD bool   `json:"can_edit_group_md"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.Len(t, got.Data, 3)

	type listItem struct {
		Role           int
		MemberCount    int
		CanEditGroupMD bool
	}
	items := make(map[string]listItem, len(got.Data))
	for _, item := range got.Data {
		items[item.GroupNo] = listItem{
			Role:           item.Role,
			MemberCount:    item.MemberCount,
			CanEditGroupMD: item.CanEditGroupMD,
		}
	}

	manager, ok := items[managerGroupNo]
	require.True(t, ok, "manager group missing from list: %s", response.Body.String())
	require.Equal(t, group.MemberRoleManager, manager.Role)
	require.Equal(t, 3, manager.MemberCount)
	require.True(t, manager.CanEditGroupMD)

	admin, ok := items[adminGroupNo]
	require.True(t, ok, "bot-admin group missing from list: %s", response.Body.String())
	require.Equal(t, group.MemberRoleCommon, admin.Role)
	require.Equal(t, 2, admin.MemberCount)
	require.True(t, admin.CanEditGroupMD)

	plainGroup, ok := items[commonGroupNo]
	require.True(t, ok, "common-membership group missing from list: %s", response.Body.String())
	require.Equal(t, group.MemberRoleCommon, plainGroup.Role)
	require.Equal(t, 2, plainGroup.MemberCount)
	require.False(t, plainGroup.CanEditGroupMD)
}

// seedCLIGroupThread inserts a live thread row so both GROUP.md permission
// cases can vary only the thread creator and the Bot's bot_admin flag.
func seedCLIGroupThread(t *testing.T, ctx *config.Context, groupNo, shortID, creatorUID, name string) {
	t.Helper()
	require.NoError(t, thread.NewDB(ctx).Insert(&thread.Model{
		ShortID:    shortID,
		GroupNo:    groupNo,
		Name:       name,
		CreatorUID: creatorUID,
		Status:     thread.ThreadStatusActive,
		Version:    1,
	}))
}

// seedCLIGroupRobot gives the inviter a valid robot row so checkBotOwnership
// admits ownership before any Space gate runs. status=1 is the same
// LEFT JOIN predicate the ownership check and v1 handlers rely on.
func seedCLIGroupRobot(t *testing.T, ctx *config.Context, robotID, ownerID string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("robot").Columns("robot_id", "status", "creator_uid").
		Values(robotID, 1, ownerID).Exec()
	require.NoError(t, err)
}

// TestCLIThreadUpdateMdRejectsBotCreatorWithoutAdmin pins the old Bot thread
// endpoint's gate onto the v2 route: the Bot's bot_admin flag, not its thread
// creator identity, is the authority for a Bot writing a thread GROUP.md. A
// Bot that created the thread but is not the group's bot_admin must be refused
// and must leave the stored markdown and author untouched.
func TestCLIThreadUpdateMdRejectsBotCreatorWithoutAdmin(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_md_creator_owner"
	const botID = "cli_thread_md_creator_bot"
	const spaceID = "cli_thread_md_creator_space"
	const groupNo = "g_cli_thread_md_creator"
	const threadID = "123456789012345678"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 0, 1)
	seedCLIGroupThread(t, ctx, groupNo, threadID, botID, "Bot-created topic")

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPut,
		"/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/md",
		`{"content":"# bot creator must not write"}`)
	require.Equalf(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"FORBIDDEN"`)

	var stored struct {
		Content   string `db:"content"`
		UpdatedBy string `db:"updated_by"`
	}
	_, err := ctx.DB().SelectBySql(
		"SELECT IFNULL(thread_md,'') AS content, thread_md_updated_by AS updated_by FROM thread WHERE group_no=? AND short_id=?",
		groupNo, threadID,
	).Load(&stored)
	require.NoError(t, err)
	require.Empty(t, stored.Content)
	require.Empty(t, stored.UpdatedBy)
}

// TestCLIThreadUpdateMdAllowsBotAdminNonCreator is the positive side of the
// same v1 parity gate: a Bot that did not create the thread but holds
// bot_admin=1 may write it, and the response version plus stored author must
// come from that Bot.
func TestCLIThreadUpdateMdAllowsBotAdminNonCreator(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_md_admin_owner"
	const botID = "cli_thread_md_admin_bot"
	const spaceID = "cli_thread_md_admin_space"
	const groupNo = "g_cli_thread_md_admin"
	const threadID = "123456789012345679"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, groupNo, botID, 1, 1)
	seedCLIGroupThread(t, ctx, groupNo, threadID, ownerID, "Owner-created topic")

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()
	response := cliGroupRequest(t, router, http.MethodPut,
		"/v2/cli/groups/"+groupNo+"/threads/"+threadID+"/md",
		`{"content":"# bot admin markdown"}`)
	require.Equalf(t, http.StatusOK, response.Code, response.Body.String())

	var got struct {
		Data struct {
			Version int64 `json:"version"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	require.EqualValues(t, 1, got.Data.Version)

	var stored struct {
		Content   string `db:"content"`
		UpdatedBy string `db:"updated_by"`
	}
	_, err := ctx.DB().SelectBySql(
		"SELECT IFNULL(thread_md,'') AS content, thread_md_updated_by AS updated_by FROM thread WHERE group_no=? AND short_id=?",
		groupNo, threadID,
	).Load(&stored)
	require.NoError(t, err)
	require.Equal(t, "# bot admin markdown", stored.Content)
	require.Equal(t, botID, stored.UpdatedBy)
}

// TestCLIThreadCreateAsBotRejectsCallerSourceMessagePayload keeps the Bot
// subset of the legacy create contract: the v1 Bot endpoint accepts
// source_message_id but has no source_message_payload field, because the
// service forwards that caller-supplied payload under the source message's
// real sender. A Bot request carrying it must be refused before a thread row
// exists; a source_message_id without the payload remains accepted.
func TestCLIThreadCreateAsBotRejectsCallerSourceMessagePayload(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_thread_payload_owner"
	const botID = "cli_thread_payload_bot"
	const humanID = "cli_thread_payload_human"
	const spaceID = "cli_thread_payload_space"
	const groupNo = "g_cli_thread_payload"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupUser(t, ctx, humanID, "Human", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID, humanID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID, botID, humanID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()

	impersonating := `{"name":"Bot impersonation topic","source_message_id":12345,` +
		`"source_message_payload":{"type":1,"from_uid":"cli_thread_payload_owner",` +
		`"from_name":"Owner","content":"impersonated text"}}`
	response := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/threads", impersonating)
	require.Equalf(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"VALIDATION_ERROR"`)

	var count int
	err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM thread WHERE group_no=? AND name=?", groupNo, "Bot impersonation topic",
	).LoadOne(&count)
	require.NoError(t, err)
	require.Zero(t, count)

	accepted := cliGroupRequest(t, router, http.MethodPost,
		"/v2/cli/groups/"+groupNo+"/threads",
		`{"name":"Bot source topic","source_message_id":12345}`)
	require.Equalf(t, http.StatusOK, accepted.Code, accepted.Body.String())

	var sourceMessageID int64
	err = ctx.DB().SelectBySql(
		"SELECT source_message_id FROM thread WHERE group_no=? AND name=?", groupNo, "Bot source topic",
	).LoadOne(&sourceMessageID)
	require.NoError(t, err)
	require.EqualValues(t, 12345, sourceMessageID)
}

// TestCLIGroupMemberAddAsUserRejectsBotOutsideInviterSpace preserves the v1
// Bot/Space isolation rule for a human inviter: a Bot owned by the inviter is
// still invalid when it is not an active member of the inviter's effective
// Space. The in-Space Bot in the same test proves ownership passes and the
// rejection is specifically the Space gate.
func TestCLIGroupMemberAddAsUserRejectsBotOutsideInviterSpace(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_space_bot_invite_owner"
	const insideBotID = "cli_space_bot_invite_inside"
	const outsideBotID = "cli_space_bot_invite_outside"
	const spaceID = "cli_space_bot_invite_space"
	const groupNo = "g_cli_space_bot_invite"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, insideBotID, "Inside Bot", 1)
	seedCLIGroupUser(t, ctx, outsideBotID, "Outside Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, insideBotID)
	seedCLIGroupRobot(t, ctx, insideBotID, ownerID)
	seedCLIGroupRobot(t, ctx, outsideBotID, ownerID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()
	addPath := "/v2/cli/groups/" + groupNo + "/members/_add"

	rejected := cliGroupRequest(t, router, http.MethodPost, addPath,
		fmt.Sprintf(`{"members":["%s"]}`, outsideBotID))
	require.Equalf(t, http.StatusForbidden, rejected.Code, rejected.Body.String())
	require.Contains(t, rejected.Body.String(), `"FORBIDDEN"`)

	var outsideCount int
	err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, outsideBotID,
	).LoadOne(&outsideCount)
	require.NoError(t, err)
	require.Zero(t, outsideCount)

	accepted := cliGroupRequest(t, router, http.MethodPost, addPath,
		fmt.Sprintf(`{"members":["%s"]}`, insideBotID))
	require.Equalf(t, http.StatusOK, accepted.Code, accepted.Body.String())
	var added struct {
		Data struct {
			Added int `json:"added"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(accepted.Body.Bytes(), &added))
	require.Equal(t, 1, added.Data.Added)

	var insideCount int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no=? AND uid=? AND is_deleted=0",
		groupNo, insideBotID,
	).LoadOne(&insideCount)
	require.NoError(t, err)
	require.Equal(t, 1, insideCount)
}

// TestCLIGroupDetailReportsBotMarkdownPermission keeps the detail projection
// consistent with the real Bot GROUP.md write gate. For a Bot the authority is
// bot_admin, not its native group role: bot_admin=1 with a common-role member
// row reports true, while role=manager with bot_admin=0 reports false. Each
// case is paired with the actual PUT /md outcome so the field can never drift
// from enforcement again.
func TestCLIGroupDetailReportsBotMarkdownPermission(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_detail_md_owner"
	const botID = "cli_detail_md_bot"
	const spaceID = "cli_detail_md_space"
	const adminGroupNo = "g_cli_detail_md_admin"
	const managerGroupNo = "g_cli_detail_md_manager"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupUser(t, ctx, botID, "Bot", 1)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID, botID)
	seedCLIGroup(t, ctx, adminGroupNo, spaceID, ownerID, botID)
	seedCLIGroup(t, ctx, managerGroupNo, spaceID, ownerID, botID)
	setCLIGroupMemberFlags(t, ctx, adminGroupNo, botID, 1, 1)
	setCLIGroupMemberFlags(t, ctx, managerGroupNo, botID, 0, 1)
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET role=? WHERE group_no=? AND uid=?",
		group.MemberRoleManager, managerGroupNo, botID,
	).Exec()
	require.NoError(t, err)

	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, botID, "Bot")
	defer closeAuth()

	requireDetail := func(groupNo string, want bool) {
		t.Helper()
		response := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups/"+groupNo, "")
		require.Equalf(t, http.StatusOK, response.Code, response.Body.String())
		var detail struct {
			Data struct {
				CanEditGroupMD bool `json:"can_edit_group_md"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &detail))
		require.Equalf(t, want, detail.Data.CanEditGroupMD, "group %s detail: %s", groupNo, response.Body.String())
	}
	requireDetail(adminGroupNo, true)
	requireDetail(managerGroupNo, false)

	list := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups?page_size=100", "")
	require.Equalf(t, http.StatusOK, list.Code, list.Body.String())
	var listed struct {
		Data []struct {
			GroupNo        string `json:"group_no"`
			CanEditGroupMD bool   `json:"can_edit_group_md"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listed))
	permissions := make(map[string]bool, len(listed.Data))
	for _, item := range listed.Data {
		permissions[item.GroupNo] = item.CanEditGroupMD
	}
	require.Equal(t, map[string]bool{adminGroupNo: true, managerGroupNo: false}, permissions)

	adminWrite := cliGroupRequest(t, router, http.MethodPut,
		"/v2/cli/groups/"+adminGroupNo+"/md", `{"content":"# bot admin writes"}`)
	require.Equalf(t, http.StatusOK, adminWrite.Code, adminWrite.Body.String())

	managerWrite := cliGroupRequest(t, router, http.MethodPut,
		"/v2/cli/groups/"+managerGroupNo+"/md", `{"content":"# bot manager must not write"}`)
	require.Equalf(t, http.StatusForbidden, managerWrite.Code, managerWrite.Body.String())
}

func TestCLIGroupListReportsBotAdminManagementPermission(t *testing.T) {
	source, ctx := newCLIGroupTestSource(t)
	const ownerID = "cli_list_manage_owner"
	const spaceID = "cli_list_manage_space"
	const groupNo = "g_cli_list_manage"
	seedCLIGroupUser(t, ctx, ownerID, "Owner", 0)
	seedCLIGroupSpace(t, ctx, spaceID, ownerID, ownerID)
	seedCLIGroup(t, ctx, groupNo, spaceID, ownerID)
	router, closeAuth := newCLIGroupTestRouter(t, source, ownerID, "Owner", spaceID, "", "")
	defer closeAuth()

	list := cliGroupRequest(t, router, http.MethodGet, "/v2/cli/groups", "")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var result struct {
		Data []struct {
			GroupNo           string `json:"group_no"`
			CanManageBotAdmin bool   `json:"can_manage_bot_admin"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &result))
	require.Len(t, result.Data, 1)
	require.Equal(t, groupNo, result.Data[0].GroupNo)
	require.True(t, result.Data[0].CanManageBotAdmin)
}
