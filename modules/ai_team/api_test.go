package ai_team_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	aiteammod "github.com/Mininglamp-OSS/octo-server/modules/ai_team"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testServer  *server.Server
	testContext *config.Context
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.ReleaseMode)
	_ = os.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_ = os.Setenv("DM_AI_TEAM_ON", "true")
	_ = os.Setenv("DM_THREAD_ON", "true")
	testServer, testContext = testutil.NewTestServer()
	testServer.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	os.Exit(m.Run())
}

func resetState(t *testing.T) {
	t.Helper()
	require.NoError(t, testutil.CleanAllTables(testContext))
	client := redis.NewClient(&redis.Options{
		Addr:     testContext.GetConfig().DB.RedisAddr,
		Password: testContext.GetConfig().DB.RedisPass,
	})
	t.Cleanup(func() { _ = client.Close() })
	keys, err := client.Keys("ratelimit:uid:*").Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, client.Del(keys...).Err())
	}
}

type fixture struct {
	uid     string
	botID   string
	spaceID string
	token   string
}

func seedFixture(t *testing.T) fixture {
	t.Helper()
	resetState(t)
	suffix := util.GenerUUID()[:8]
	f := fixture{
		uid:     "ai_owner_" + suffix,
		botID:   "ai_bot_" + suffix,
		spaceID: "ai_space_" + suffix,
		token:   "ai_token_" + suffix,
	}
	for _, u := range []struct{ uid, name string }{{f.uid, "AI owner"}, {f.botID, "Assistant"}} {
		_, err := testContext.DB().InsertBySql(
			"INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)",
			u.uid, u.name, u.uid).Exec()
		require.NoError(t, err)
	}
	_, err := testContext.DB().InsertBySql(
		"INSERT INTO `space` (space_id,name,creator,status) VALUES (?,?,?,1)",
		f.spaceID, "AI space", f.uid).Exec()
	require.NoError(t, err)
	for _, uid := range []string{f.uid, f.botID} {
		_, err = testContext.DB().InsertBySql(
			"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, uid).Exec()
		require.NoError(t, err)
	}
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO robot (robot_id,creator_uid,status) VALUES (?,?,1)", f.botID, f.uid).Exec()
	require.NoError(t, err)
	require.NoError(t, testContext.Cache().Set(testContext.GetConfig().Cache.TokenCachePrefix+f.token, f.uid+"@test"))
	return f
}

func request(t *testing.T, f fixture, method, path, idem string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", f.token)
	req.Header.Set("X-Space-ID", f.spaceID)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	w := httptest.NewRecorder()
	testServer.GetRoute().ServeHTTP(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), out), "body=%s", w.Body.String())
}

func TestAITeamAgentLifecycleAndSessionIdempotency(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var emptyPage aiteammod.AgentPage
	decodeJSON(t, w, &emptyPage)
	require.Len(t, emptyPage.Groups, 3)
	assert.Equal(t, aiteammod.AgentGroupTypeCloudClone, emptyPage.Groups[0].Type)
	assert.Equal(t, aiteammod.AgentGroupTypePersonalAssistant, emptyPage.Groups[1].Type)
	assert.Equal(t, aiteammod.AgentGroupTypeDigitalEmployee, emptyPage.Groups[2].Type)
	assert.Zero(t, emptyPage.Groups[0].Count)
	assert.EqualValues(t, 1, emptyPage.Groups[1].Count)
	assert.Equal(t, []string{f.botID}, agentBotIDs(emptyPage.Groups[1].Items))
	assert.Zero(t, emptyPage.Groups[2].Count)
	require.NotNil(t, emptyPage.TeamGroup)
	require.NotEmpty(t, emptyPage.TeamGroup.GroupNo)
	assert.Equal(t, aiteampkg.TeamGroupName, emptyPage.TeamGroup.Name)
	assert.Equal(t, 2, emptyPage.TeamGroup.State)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first struct {
		BotID          string `json:"bot_id"`
		GroupNo        string `json:"group_no"`
		ContainerState int    `json:"container_state"`
	}
	decodeJSON(t, w, &first)
	assert.Equal(t, f.botID, first.BotID)
	require.NotEmpty(t, first.GroupNo, "adding an AI must eagerly create its private group")
	assert.Equal(t, 2, first.ContainerState)

	var teamMembers []string
	_, err := testContext.DB().Select("uid").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", emptyPage.TeamGroup.GroupNo).
		OrderBy("uid").Load(&teamMembers)
	require.NoError(t, err)
	assert.Equal(t, []string{f.botID, f.uid}, teamMembers)

	var eagerGroupCount, eagerMemberCount, eagerSessionCount, eagerThreadCount int
	var eagerMemberUIDs []string
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("`group`").
		Where("group_no=? AND space_id=? AND creator=? AND purpose=?", first.GroupNo, f.spaceID, f.uid, "ai_session_container").
		LoadOne(&eagerGroupCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", first.GroupNo).LoadOne(&eagerMemberCount))
	_, err = testContext.DB().Select("uid").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", first.GroupNo).OrderBy("uid").Load(&eagerMemberUIDs)
	require.NoError(t, err)
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_session").LoadOne(&eagerSessionCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("thread").Where("group_no=?", first.GroupNo).LoadOne(&eagerThreadCount))
	assert.Equal(t, 1, eagerGroupCount)
	assert.Equal(t, 2, eagerMemberCount)
	assert.Equal(t, []string{f.botID, f.uid}, eagerMemberUIDs)
	assert.Zero(t, eagerSessionCount)
	assert.Zero(t, eagerThreadCount)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var repeated struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &repeated)
	assert.Equal(t, first.GroupNo, repeated.GroupNo)
	w = request(t, f, http.MethodGet, "/v1/ai-team/agents/"+f.botID+"/sessions", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"items":[]`)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "idem-1", map[string]string{"name": "First"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var session1 struct {
		SessionID string `json:"session_id"`
		GroupNo   string `json:"group_no"`
		ChannelID string `json:"channel_id"`
	}
	decodeJSON(t, w, &session1)
	require.NotEmpty(t, session1.SessionID)
	require.NotEmpty(t, session1.GroupNo)
	assert.Equal(t, first.GroupNo, session1.GroupNo)
	assert.Equal(t, session1.GroupNo+"____"+session1.SessionID, session1.ChannelID)

	target, err := aiteampkg.LookupReadySessionTarget(testContext.DB(), session1.ChannelID, f.uid)
	require.NoError(t, err)
	require.NotNil(t, target)
	assert.Equal(t, f.botID, target.BotID)
	target, err = aiteampkg.LookupReadySessionTarget(testContext.DB(), session1.ChannelID, "another-user")
	require.NoError(t, err)
	assert.Nil(t, target, "a different user must not resolve the private session target")
	_, err = testContext.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)
	target, err = aiteampkg.LookupReadySessionTarget(testContext.DB(), session1.ChannelID, f.uid)
	require.NoError(t, err)
	assert.Nil(t, target, "an inactive Space seat must stop automatic Bot routing")
	_, err = testContext.DB().Update("space_member").Set("status", 1).
		Where("space_id=? AND uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)

	other := f
	other.uid = "other_" + util.GenerUUID()[:8]
	other.token = "other_token_" + util.GenerUUID()[:8]
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)",
		other.uid, "Other user", other.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, other.uid).Exec()
	require.NoError(t, err)
	require.NoError(t, testContext.Cache().Set(testContext.GetConfig().Cache.TokenCachePrefix+other.token, other.uid+"@test"))
	w = request(t, other, http.MethodGet, "/v1/ai-team/sessions/"+session1.SessionID, "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "idem-1", map[string]string{"name": "First"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var replay struct {
		SessionID string `json:"session_id"`
		GroupNo   string `json:"group_no"`
	}
	decodeJSON(t, w, &replay)
	assert.Equal(t, session1.SessionID, replay.SessionID)
	assert.Equal(t, session1.GroupNo, replay.GroupNo)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "idem-1", map[string]string{"name": "Different"})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.idempotency_conflict")

	var groupCount, memberCount, sessionCount int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("`group`").Where("group_no=? AND purpose=?", session1.GroupNo, "ai_session_container").LoadOne(&groupCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").Where("group_no=? AND status=1 AND is_deleted=0", session1.GroupNo).LoadOne(&memberCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_session").LoadOne(&sessionCount))
	assert.Equal(t, 1, groupCount)
	assert.Equal(t, 2, memberCount)
	assert.Equal(t, 1, sessionCount)

	w = request(t, f, http.MethodDelete, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	teamMembers = nil
	_, err = testContext.DB().Select("uid").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", emptyPage.TeamGroup.GroupNo).
		OrderBy("uid").Load(&teamMembers)
	require.NoError(t, err)
	assert.Equal(t, []string{f.uid}, teamMembers)
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var restored struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &restored)
	assert.Equal(t, session1.GroupNo, restored.GroupNo)
	teamMembers = nil
	_, err = testContext.DB().Select("uid").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", emptyPage.TeamGroup.GroupNo).
		OrderBy("uid").Load(&teamMembers)
	require.NoError(t, err)
	assert.Equal(t, []string{f.botID, f.uid}, teamMembers)

	// Model the authoritative Space-removal aftermath: the owner and their Bot
	// have been soft-removed from the parent while the durable agent/session rows
	// remain. Re-adding the Space seat and AI must repair both DB membership and
	// every existing WuKongIM channel before returning the historical session.
	_, err = testContext.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().Update("group_member").Set("is_deleted", 1).
		Where("group_no=? AND uid IN ?", session1.GroupNo, []string{f.uid, f.botID}).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().Update("space_member").Set("status", 1).
		Where("space_id=? AND uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)

	var mu sync.Mutex
	var reprovisioned []config.ChannelCreateReq
	imStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel" {
			body, _ := io.ReadAll(r.Body)
			var call config.ChannelCreateReq
			_ = json.Unmarshal(body, &call)
			mu.Lock()
			reprovisioned = append(reprovisioned, call)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer imStub.Close()
	previousIMURL := testContext.GetConfig().WuKongIM.APIURL
	testContext.GetConfig().WuKongIM.APIURL = imStub.URL
	defer func() { testContext.GetConfig().WuKongIM.APIURL = previousIMURL }()

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var activeUIDs []string
	_, err = testContext.DB().Select("uid").From("group_member").
		Where("group_no=? AND status=1 AND is_deleted=0", session1.GroupNo).OrderBy("uid").Load(&activeUIDs)
	require.NoError(t, err)
	assert.Equal(t, []string{f.botID, f.uid}, activeUIDs)
	w = request(t, f, http.MethodGet, "/v1/ai-team/sessions/"+session1.SessionID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	mu.Lock()
	channels := make(map[string][]string, len(reprovisioned))
	for _, call := range reprovisioned {
		channels[call.ChannelID] = append([]string(nil), call.Subscribers...)
	}
	mu.Unlock()
	assert.ElementsMatch(t, []string{f.uid, f.botID}, channels[session1.GroupNo])
	assert.ElementsMatch(t, []string{f.uid, f.botID}, channels[session1.ChannelID])
}

func TestAITeamGroupAndAgentContainersConvergeTogether(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page aiteammod.AgentPage
	decodeJSON(t, w, &page)
	require.NotNil(t, page.TeamGroup)

	secondBot := "ai_bot_" + util.GenerUUID()[:8]
	_, err := testContext.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)",
		secondBot, "Second assistant", secondBot).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO robot (robot_id,creator_uid,status) VALUES (?,?,1)", secondBot, f.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, secondBot).Exec()
	require.NoError(t, err)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+secondBot, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var second aiteammod.Agent
	decodeJSON(t, w, &second)
	require.NotEmpty(t, second.GroupNo)

	assertMembers := func(groupNo string, want []string) {
		t.Helper()
		var got []string
		_, queryErr := testContext.DB().Select("uid").From("group_member").
			Where("group_no=? AND status=1 AND is_deleted=0", groupNo).OrderBy("uid").Load(&got)
		require.NoError(t, queryErr)
		assert.ElementsMatch(t, want, got)
	}
	assertMembers(second.GroupNo, []string{f.uid, secondBot})
	assertMembers(page.TeamGroup.GroupNo, []string{f.botID, secondBot, f.uid})

	outsider := "outsider_" + util.GenerUUID()[:8]
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)",
		outsider, "Outsider", outsider).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql(
		"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, outsider).Exec()
	require.NoError(t, err)
	_, err = group.NewService(testContext).AddGroupMembers(&group.AddGroupMembersServiceReq{
		GroupNo: page.TeamGroup.GroupNo, Members: []string{outsider}, OperatorUID: f.uid,
	})
	require.ErrorIs(t, err, aiteampkg.ErrContainerProtected)
	assertMembers(page.TeamGroup.GroupNo, []string{f.botID, secondBot, f.uid})

	w = request(t, f, http.MethodPut, "/v1/groups/"+page.TeamGroup.GroupNo+"/setting", "", map[string]any{"mute": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPut, "/v1/groups/"+page.TeamGroup.GroupNo+"/setting", "", map[string]any{"allow_no_mention": 0})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var allowNoMention int
	require.NoError(t, testContext.DB().Select("allow_no_mention").From("`group`").
		Where("group_no=?", page.TeamGroup.GroupNo).LoadOne(&allowNoMention))
	assert.Zero(t, allowNoMention)
	w = request(t, f, http.MethodPut, "/v1/groups/"+page.TeamGroup.GroupNo+"/setting", "", map[string]any{"allow_external": 1})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.container_protected")

	// A bypassed/stale row and a missing desired member are both repaired by the
	// same exact-roster projection on the next AI-team list call.
	groupDB := group.NewDB(testContext)
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: page.TeamGroup.GroupNo, UID: outsider, Role: group.MemberRoleCommon,
		Status: 1, Version: 1, Vercode: outsider + "@1",
	}))
	_, err = testContext.DB().Update("group_member").Set("is_deleted", 1).
		Where("group_no=? AND uid=?", page.TeamGroup.GroupNo, f.botID).Exec()
	require.NoError(t, err)
	w = request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assertMembers(page.TeamGroup.GroupNo, []string{f.botID, secondBot, f.uid})

	w = request(t, f, http.MethodDelete, "/v1/ai-team/agents/"+secondBot, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assertMembers(page.TeamGroup.GroupNo, []string{f.botID, f.uid})
	assertMembers(second.GroupNo, []string{f.uid, secondBot})
}

func TestAITeamConcurrentAgentActivationCreatesOnePairAndOneTeamGroup(t *testing.T) {
	f := seedFixture(t)
	svc := aiteammod.NewService(testContext)

	const workers = 6
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.AddAgent(f.spaceID, f.uid, f.botID)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	var agentCount, pairGroupCount, teamGroupCount int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).LoadOne(&agentCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("`group`").
		Where("creator=? AND space_id=? AND purpose=?", f.uid, f.spaceID, aiteampkg.GroupPurpose).LoadOne(&pairGroupCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("`group`").
		Where("creator=? AND space_id=? AND purpose=?", f.uid, f.spaceID, aiteampkg.TeamGroupPurpose).LoadOne(&teamGroupCount))
	assert.Equal(t, 1, agentCount)
	assert.Equal(t, 1, pairGroupCount)
	assert.Equal(t, 1, teamGroupCount)
}

func TestAITeamListAgentsGroupsByHostingAndKeepsCursorTotals(t *testing.T) {
	f := seedFixture(t)
	_, err := testContext.DB().Update("robot").Set("agent_hosting", "octo_hosted").
		Where("robot_id=?", f.botID).Exec()
	require.NoError(t, err)
	require.NoError(t, insertAgentForListTest(f, f.botID))

	type botSpec struct {
		suffix  string
		hosting string
	}
	botIDs := make(map[string]string)
	for _, spec := range []botSpec{
		{suffix: "self", hosting: "self_hosted"},
		{suffix: "vendor", hosting: "vendor_hosted"},
		{suffix: "empty", hosting: ""},
		{suffix: "cloud", hosting: "octo_hosted"},
	} {
		botID := "ai_" + spec.suffix + "_" + util.GenerUUID()[:8]
		botIDs[spec.suffix] = botID
		_, err = testContext.DB().InsertBySql(
			"INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)",
			botID, spec.suffix, botID).Exec()
		require.NoError(t, err)
		_, err = testContext.DB().InsertBySql(
			"INSERT INTO robot (robot_id,creator_uid,status,agent_hosting) VALUES (?,?,1,?)",
			botID, f.uid, spec.hosting).Exec()
		require.NoError(t, err)
		_, err = testContext.DB().InsertBySql(
			"INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, botID).Exec()
		require.NoError(t, err)
		require.NoError(t, insertAgentForListTest(f, botID))
	}

	w := request(t, f, http.MethodGet, "/v1/ai-team/agents?limit=3", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "agent_hosting", "hosting is classification input, not a new wire field")
	assert.NotContains(t, w.Body.String(), "agent_group", "the computed group is represented by the response envelope")
	var first aiteammod.AgentPage
	decodeJSON(t, w, &first)
	assertAgentGroupShape(t, first.Groups, 2, 3)
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, []string{botIDs["cloud"]}, agentBotIDs(first.Groups[0].Items))
	assert.Equal(t, []string{botIDs["empty"], botIDs["vendor"]}, agentBotIDs(first.Groups[1].Items))

	w = request(t, f, http.MethodGet, "/v1/ai-team/agents?limit=3&cursor="+first.NextCursor, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var second aiteammod.AgentPage
	decodeJSON(t, w, &second)
	assertAgentGroupShape(t, second.Groups, 2, 3)
	assert.Empty(t, second.NextCursor)
	assert.Equal(t, []string{f.botID}, agentBotIDs(second.Groups[0].Items))
	assert.Equal(t, []string{botIDs["self"]}, agentBotIDs(second.Groups[1].Items))
}

func insertAgentForListTest(f fixture, botID string) error {
	_, err := testContext.DB().InsertBySql(
		"INSERT INTO ai_team_agent (space_id,user_uid,bot_id,is_added) VALUES (?,?,?,1)",
		f.spaceID, f.uid, botID).Exec()
	return err
}

func assertAgentGroupShape(t *testing.T, groups []*aiteammod.AgentGroup, cloudCount, personalCount int64) {
	t.Helper()
	require.Len(t, groups, 3)
	assert.Equal(t, aiteammod.AgentGroupTypeCloudClone, groups[0].Type)
	assert.Equal(t, cloudCount, groups[0].Count)
	assert.NotNil(t, groups[0].Items)
	assert.Equal(t, aiteammod.AgentGroupTypePersonalAssistant, groups[1].Type)
	assert.Equal(t, personalCount, groups[1].Count)
	assert.NotNil(t, groups[1].Items)
	assert.Equal(t, aiteammod.AgentGroupTypeDigitalEmployee, groups[2].Type)
	assert.Zero(t, groups[2].Count)
	assert.NotNil(t, groups[2].Items)
	assert.Empty(t, groups[2].Items)
}

func agentBotIDs(agents []*aiteammod.Agent) []string {
	botIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		botIDs = append(botIDs, agent.BotID)
	}
	return botIDs
}

func TestAITeamConcurrentInitializationUsesOneParent(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	const workers = 6
	type result struct {
		session *aiteammod.Session
		err     error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := aiteammod.NewService(testContext).CreateSession(f.spaceID, f.uid, f.botID, "same-key", "Concurrent")
			results <- result{session: session, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var groupNo, shortID string
	for got := range results {
		require.NoError(t, got.err)
		if groupNo == "" {
			groupNo, shortID = got.session.GroupNo, got.session.ShortID
		}
		assert.Equal(t, groupNo, got.session.GroupNo)
		assert.Equal(t, shortID, got.session.ShortID)
	}
	var agents, groups, sessions int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_agent").LoadOne(&agents))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("`group`").Where("purpose=?", "ai_session_container").LoadOne(&groups))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_session").LoadOne(&sessions))
	assert.Equal(t, 1, agents)
	assert.Equal(t, 1, groups)
	assert.Equal(t, 1, sessions)
}

func TestAITeamCreateSessionAcceptsChunkedJSON(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	req, err := http.NewRequest(http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", strings.NewReader(`{"name":"Chunked"}`))
	require.NoError(t, err)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", f.token)
	req.Header.Set("X-Space-ID", f.spaceID)
	req.Header.Set("Idempotency-Key", "chunked-body")
	w = httptest.NewRecorder()
	testServer.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"name":"Chunked"`)
}

func TestAITeamSessionPersonalControls(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "controls-1", map[string]string{"name": "First"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first struct {
		SessionID string `json:"session_id"`
		ChannelID string `json:"channel_id"`
	}
	decodeJSON(t, w, &first)

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "controls-2", map[string]string{"name": "Second"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var second struct {
		SessionID string `json:"session_id"`
	}
	decodeJSON(t, w, &second)

	w = request(t, f, http.MethodPut, "/v1/ai-team/sessions/"+first.SessionID, "", map[string]string{"name": "Renamed"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPut, "/v1/ai-team/sessions/"+first.SessionID+"/setting", "", map[string]int{"mute": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPut, "/v1/ai-team/sessions/"+first.SessionID+"/setting", "", map[string]int{"mute": 2})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = request(t, f, http.MethodPost, "/v1/user/pinned", "", map[string]interface{}{
		"channel_id": first.ChannelID, "channel_type": common.ChannelTypeCommunityTopic.Uint8(),
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = request(t, f, http.MethodGet, "/v1/ai-team/agents/"+f.botID+"/sessions", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page struct {
		Items []struct {
			SessionID string `json:"session_id"`
			Name      string `json:"name"`
			Mute      int    `json:"mute"`
			IsPinned  bool   `json:"is_pinned"`
		} `json:"items"`
	}
	decodeJSON(t, w, &page)
	require.Len(t, page.Items, 2)
	assert.Equal(t, first.SessionID, page.Items[0].SessionID, "the pinned session must sort before a newer unpinned session")
	assert.Equal(t, "Renamed", page.Items[0].Name)
	assert.Equal(t, 1, page.Items[0].Mute)
	assert.True(t, page.Items[0].IsPinned)

	// Clear chat history reuses the existing per-user offset contract: history
	// is hidden for this owner without physically deleting either participant's messages.
	w = request(t, f, http.MethodPost, "/v1/message/offset", "", map[string]interface{}{
		"channel_id": first.ChannelID, "channel_type": common.ChannelTypeCommunityTopic.Uint8(), "message_seq": 42,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var offsetRows int
	for _, table := range []string{"channel_offset", "channel_offset1", "channel_offset2"} {
		var count int
		require.NoError(t, testContext.DB().Select("COUNT(*)").From(table).
			Where("uid=? AND channel_id=? AND channel_type=? AND message_seq=?", f.uid, first.ChannelID, common.ChannelTypeCommunityTopic.Uint8(), 42).
			LoadOne(&count))
		offsetRows += count
	}
	assert.Equal(t, 1, offsetRows)

	w = request(t, f, http.MethodDelete, "/v1/ai-team/sessions/"+first.SessionID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodGet, "/v1/ai-team/sessions/"+first.SessionID, "", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "controls-1", map[string]string{"name": "First"})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.idempotency_conflict")
	var status int
	require.NoError(t, testContext.DB().Select("status").From("thread").Where("short_id=?", first.SessionID).LoadOne(&status))
	assert.Equal(t, 3, status, "session deletion must remain a recoverable DB soft delete")

	w = request(t, f, http.MethodGet, "/v1/ai-team/agents/"+f.botID+"/sessions", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	decodeJSON(t, w, &page)
	require.Len(t, page.Items, 1)
	assert.Equal(t, second.SessionID, page.Items[0].SessionID)
}

func TestAITeamSessionProvisionFailureDoesNotPoisonReadyContainer(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "ready-session", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	failingIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forced IM failure", http.StatusInternalServerError)
	}))
	defer failingIM.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failingIM.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "failed-session", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.im_unavailable")

	var containerState int
	require.NoError(t, testContext.DB().Select("container_state").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).
		LoadOne(&containerState))
	assert.Equal(t, 2, containerState, "one failed thread must not downgrade a ready parent")
}

func TestAITeamAddAgentProvisionFailureIsRetryable(t *testing.T) {
	f := seedFixture(t)
	failingIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forced IM failure", http.StatusInternalServerError)
	}))
	defer failingIM.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failingIM.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.im_unavailable")

	var persisted struct {
		GroupNo        string `db:"group_no"`
		ContainerState int    `db:"container_state"`
	}
	var sessionCount, threadCount int
	require.NoError(t, testContext.DB().Select("group_no", "container_state").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).
		LoadOne(&persisted))
	require.NotEmpty(t, persisted.GroupNo)
	assert.Equal(t, 3, persisted.ContainerState)
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("ai_team_session").LoadOne(&sessionCount))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("thread").Where("group_no=?", persisted.GroupNo).LoadOne(&threadCount))
	assert.Zero(t, sessionCount)
	assert.Zero(t, threadCount)

	var provisioned []config.ChannelCreateReq
	successIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel" {
			var req config.ChannelCreateReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			provisioned = append(provisioned, req)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer successIM.Close()
	cfg.WuKongIM.APIURL = successIM.URL
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var retried struct {
		GroupNo        string `json:"group_no"`
		ContainerState int    `json:"container_state"`
	}
	decodeJSON(t, w, &retried)
	assert.Equal(t, persisted.GroupNo, retried.GroupNo)
	assert.Equal(t, 2, retried.ContainerState)
	var privateGroup *config.ChannelCreateReq
	for i := range provisioned {
		if provisioned[i].ChannelID == persisted.GroupNo {
			privateGroup = &provisioned[i]
			break
		}
	}
	require.NotNil(t, privateGroup)
	assert.Equal(t, common.ChannelTypeGroup.Uint8(), privateGroup.ChannelType)
	assert.ElementsMatch(t, []string{f.uid, f.botID}, privateGroup.Subscribers)
}

func TestAITeamManagedGroupsDoNotConsumeDailyGroupCreationQuota(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	cfg := testContext.GetConfig()
	previousLimit := cfg.Group.SameDayCreateMaxCount
	cfg.Group.SameDayCreateMaxCount = 1
	t.Cleanup(func() { cfg.Group.SameDayCreateMaxCount = previousLimit })

	w = request(t, f, http.MethodPost, "/v1/group/create", "", map[string]any{
		"name": "ordinary group after AI provisioning", "members": []string{f.botID}, "space_id": f.spaceID,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestAITeamListSkipsReadyIMReplayAndDegradesRepairFailure(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var calls int
	failingIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "forced IM failure", http.StatusInternalServerError)
	}))
	defer failingIM.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failingIM.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	// A fully ready page load remains a read: no external IM replay at all.
	w = request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Zero(t, calls)

	// A persisted failed state triggers repair, but an unavailable IM service
	// must not blank the list. The degraded state is returned to the client.
	_, err := testContext.DB().Update("ai_team_agent").Set("container_state", 3).
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).Exec()
	require.NoError(t, err)
	w = request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Positive(t, calls)
	var page aiteammod.AgentPage
	decodeJSON(t, w, &page)
	require.Len(t, page.Groups[1].Items, 1)
	assert.Equal(t, 3, page.Groups[1].Items[0].ContainerState)
}

func TestAITeamProjectionReleasesDBBeforeIMAndReplaysStaleRoster(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var teamGroupNo string
	require.NoError(t, testContext.DB().Select("group_no").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&teamGroupNo))
	require.NotEmpty(t, teamGroupNo)

	var channelCalls atomic.Int32
	var rosterMu sync.Mutex
	var appliedRosters [][]string
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel" {
			var channel config.ChannelCreateReq
			require.NoError(t, json.NewDecoder(r.Body).Decode(&channel))
			call := channelCalls.Add(1)
			if call == 1 {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			rosterMu.Lock()
			appliedRosters = append(appliedRosters, append([]string(nil), channel.Subscribers...))
			rosterMu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer stub.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	_, err := testContext.DB().Update("ai_team_group").Set("state", 3).Set("retry_after", nil).
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)

	db := testContext.DB().Connection
	previousMaxOpen := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	defer db.SetMaxOpenConns(previousMaxOpen)

	req, err := http.NewRequest(http.MethodGet, "/v1/ai-team/agents", nil)
	require.NoError(t, err)
	req.Header.Set("token", f.token)
	req.Header.Set("X-Space-ID", f.spaceID)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		testServer.GetRoute().ServeHTTP(response, req)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("projection did not reach WuKongIM; a DB connection may still be pinned by the projection lock")
	}

	queryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var versionDuringHTTP uint64
	err = db.QueryRowContext(queryCtx, `SELECT roster_version FROM ai_team_group
		WHERE space_id=? AND user_uid=?`, f.spaceID, f.uid).Scan(&versionDuringHTTP)
	require.NoError(t, err, "WuKongIM HTTP must not hold the only pooled DB connection")

	// Drive a real concurrent roster change. Its DB transaction and newer IM
	// projection must both complete while the older IM call is still blocked.
	w = request(t, f, http.MethodDelete, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("projection did not replay and converge after the stale HTTP request completed")
	}
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.EqualValues(t, 3, channelCalls.Load(), "the stale HTTP completion must replay the newer roster")
	rosterMu.Lock()
	require.Len(t, appliedRosters, 3)
	assert.ElementsMatch(t, []string{f.uid}, appliedRosters[0], "the concurrent removal projects first")
	assert.ElementsMatch(t, []string{f.uid, f.botID}, appliedRosters[1], "the blocked stale roster lands later")
	assert.ElementsMatch(t, []string{f.uid}, appliedRosters[2], "the stale caller must restore the latest roster")
	rosterMu.Unlock()

	var final struct {
		State         int    `db:"state"`
		RosterVersion uint64 `db:"roster_version"`
	}
	require.NoError(t, testContext.DB().Select("state", "roster_version").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&final))
	assert.Equal(t, 2, final.State)
	assert.Equal(t, versionDuringHTTP+1, final.RosterVersion)

	var isAdded, activeMember int
	require.NoError(t, testContext.DB().Select("is_added").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).LoadOne(&isAdded))
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").
		Where("group_no=? AND uid=? AND is_deleted=0 AND status=1", teamGroupNo, f.botID).LoadOne(&activeMember))
	assert.Zero(t, isAdded)
	assert.Zero(t, activeMember)
}

func TestAITeamProjectionRechecksEligibilityBeforeMarkingReady(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var channelCalls atomic.Int32
	var rosterMu sync.Mutex
	var appliedRosters [][]string
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel" {
			var channel config.ChannelCreateReq
			require.NoError(t, json.NewDecoder(r.Body).Decode(&channel))
			if channelCalls.Add(1) == 1 {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			rosterMu.Lock()
			appliedRosters = append(appliedRosters, append([]string(nil), channel.Subscribers...))
			rosterMu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer stub.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	_, err := testContext.DB().Update("ai_team_group").Set("state", 3).Set("retry_after", nil).
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "/v1/ai-team/agents", nil)
	require.NoError(t, err)
	req.Header.Set("token", f.token)
	req.Header.Set("X-Space-ID", f.spaceID)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		testServer.GetRoute().ServeHTTP(response, req)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("projection did not reach WuKongIM")
	}
	// Eligibility is part of the authoritative roster even when no AI Team
	// mutation increments roster_version. The ready CAS must re-read it.
	_, err = testContext.DB().Update("robot").Set("status", 0).Where("robot_id=?", f.botID).Exec()
	require.NoError(t, err)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("projection did not replay after eligibility changed")
	}
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 2, channelCalls.Load())
	rosterMu.Lock()
	require.Len(t, appliedRosters, 2)
	assert.ElementsMatch(t, []string{f.uid, f.botID}, appliedRosters[0])
	assert.ElementsMatch(t, []string{f.uid}, appliedRosters[1])
	rosterMu.Unlock()

	var state int
	require.NoError(t, testContext.DB().Select("state").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&state))
	assert.Equal(t, 2, state)
}

func TestAITeamLifecycleRemovalCannotBeOverwrittenByStaleProjection(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var channelCalls atomic.Int32
	var rosterMu sync.Mutex
	var appliedRosters [][]string
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/channel" {
			var channel config.ChannelCreateReq
			require.NoError(t, json.NewDecoder(r.Body).Decode(&channel))
			if channelCalls.Add(1) == 1 {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			rosterMu.Lock()
			appliedRosters = append(appliedRosters, append([]string(nil), channel.Subscribers...))
			rosterMu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer stub.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	_, err := testContext.DB().Update("ai_team_group").Set("state", 3).Set("retry_after", nil).
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)
	var initialVersion uint64
	require.NoError(t, testContext.DB().Select("roster_version").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&initialVersion))

	req, err := http.NewRequest(http.MethodGet, "/v1/ai-team/agents", nil)
	require.NoError(t, err)
	req.Header.Set("token", f.token)
	req.Header.Set("X-Space-ID", f.spaceID)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		testServer.GetRoute().ServeHTTP(response, req)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("projection did not reach WuKongIM")
	}
	require.NoError(t, group.NewService(testContext).RemoveUserFromGroupsForLifecycleCleanup(f.botID))
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale projection did not replay after lifecycle cleanup")
	}
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 2, channelCalls.Load())
	rosterMu.Lock()
	require.Len(t, appliedRosters, 2)
	assert.ElementsMatch(t, []string{f.uid, f.botID}, appliedRosters[0])
	assert.ElementsMatch(t, []string{f.uid}, appliedRosters[1])
	rosterMu.Unlock()

	var final struct {
		State         int    `db:"state"`
		RosterVersion uint64 `db:"roster_version"`
	}
	require.NoError(t, testContext.DB().Select("state", "roster_version").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&final))
	assert.Equal(t, 2, final.State)
	assert.Greater(t, final.RosterVersion, initialVersion)
	var isAdded int
	require.NoError(t, testContext.DB().Select("is_added").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).LoadOne(&isAdded))
	assert.Zero(t, isAdded)
}

func TestAITeamFailedTeamProjectionUsesReadCooldown(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var calls atomic.Int32
	failingIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "forced IM failure", http.StatusInternalServerError)
	}))
	defer failingIM.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failingIM.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	_, err := testContext.DB().Update("ai_team_group").Set("state", 3).Set("retry_after", nil).
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).Exec()
	require.NoError(t, err)
	w = request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	firstCalls := calls.Load()
	require.Positive(t, firstCalls)

	var failed struct {
		State      int        `db:"state"`
		RetryAfter *time.Time `db:"retry_after"`
	}
	require.NoError(t, testContext.DB().Select("state", "retry_after").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&failed))
	assert.Equal(t, 3, failed.State)
	require.NotNil(t, failed.RetryAfter)
	assert.True(t, failed.RetryAfter.After(time.Now()))

	w = request(t, f, http.MethodGet, "/v1/ai-team/agents", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, firstCalls, calls.Load(), "read cooldown must suppress immediate IM replay")
}
func TestAITeamRemoveSurfacesPendingProjectionAfterDurableMutation(t *testing.T) {
	f := seedFixture(t)
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	failingIM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forced IM failure", http.StatusInternalServerError)
	}))
	defer failingIM.Close()
	cfg := testContext.GetConfig()
	previousURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = failingIM.URL
	defer func() { cfg.WuKongIM.APIURL = previousURL }()

	w = request(t, f, http.MethodDelete, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.im_unavailable")
	var isAdded, state int
	require.NoError(t, testContext.DB().Select("is_added").From("ai_team_agent").
		Where("space_id=? AND user_uid=? AND bot_id=?", f.spaceID, f.uid, f.botID).LoadOne(&isAdded))
	require.NoError(t, testContext.DB().Select("state").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&state))
	assert.Zero(t, isAdded)
	assert.Equal(t, 3, state)
}

func TestAITeamRejectsMissingSpaceForeignBotAndOrdinaryMutation(t *testing.T) {
	f := seedFixture(t)

	req, err := http.NewRequest(http.MethodPost, "/v1/ai-team/agents/"+f.botID, nil)
	require.NoError(t, err)
	req.Header.Set("token", f.token)
	w := httptest.NewRecorder()
	testServer.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	foreignBot := "foreign_" + util.GenerUUID()[:8]
	_, err = testContext.DB().InsertBySql("INSERT INTO `user` (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)", foreignBot, foreignBot, foreignBot).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO robot (robot_id,creator_uid,status) VALUES (?,?,1)", foreignBot, "another-owner").Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, foreignBot).Exec()
	require.NoError(t, err)
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+foreignBot, "", nil)
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.forbidden")

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var teamGroupNo string
	require.NoError(t, testContext.DB().Select("group_no").From("ai_team_group").
		Where("space_id=? AND user_uid=?", f.spaceID, f.uid).LoadOne(&teamGroupNo))
	require.NotEmpty(t, teamGroupNo)
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "guard-key", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &created)
	for _, groupNo := range []string{created.GroupNo, teamGroupNo} {
		w = request(t, f, http.MethodPost, fmt.Sprintf("/v1/groups/%s/members", groupNo), "", map[string]any{"members": []string{foreignBot}})
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "err.server.ai_team.container_protected")
	}

	w = request(t, f, http.MethodPost, fmt.Sprintf("/v1/groups/%s/members_delete", created.GroupNo), "", map[string]any{"members": []string{f.botID}})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.container_protected")
	for _, mutation := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPut, fmt.Sprintf("/v1/groups/%s/setting", created.GroupNo), map[string]any{}},
		{http.MethodDelete, fmt.Sprintf("/v1/groups/%s/disband", created.GroupNo), nil},
		{http.MethodPost, fmt.Sprintf("/v1/groups/%s/threads", created.GroupNo), map[string]any{"name": "forbidden"}},
		{http.MethodPut, fmt.Sprintf("/v1/groups/%s/welcome", created.GroupNo), map[string]any{"enabled": true, "content": "forbidden"}},
		{http.MethodGet, fmt.Sprintf("/v1/groups/%s/scanjoin?auth_code=forged", created.GroupNo), nil},
	} {
		w = request(t, f, mutation.method, mutation.path, "", mutation.body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s %s: %s", mutation.method, mutation.path, w.Body.String())
		assert.Contains(t, w.Body.String(), "err.server.ai_team.container_protected")
	}

	var memberCount int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").Where("group_no=? AND is_deleted=0", created.GroupNo).LoadOne(&memberCount))
	assert.Equal(t, 2, memberCount)
}

func TestAITeamMigrationPreservesGlobalThreadAutoArchive(t *testing.T) {
	dbName := "octo_ai_team_migration_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
	admin, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`") })

	db, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/"+dbName+"?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE system_setting (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		category VARCHAR(64) NOT NULL, key_name VARCHAR(128) NOT NULL,
		value TEXT NOT NULL, value_type VARCHAR(16) NOT NULL DEFAULT 'string',
		description VARCHAR(255) NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_category_key (category,key_name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`)
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO system_setting(category,key_name,value,value_type,description) VALUES ('thread','auto_archive_enabled','1','bool','prior value')")
	require.NoError(t, err)

	source := &migrate.FileMigrationSource{Dir: "sql"}
	_, err = migrate.Exec(db, "mysql", source, migrate.Up)
	require.NoError(t, err)
	var value string
	require.NoError(t, db.QueryRow("SELECT value FROM system_setting WHERE category='thread' AND key_name='auto_archive_enabled'").Scan(&value))
	assert.Equal(t, "1", value, "feature migration must not overwrite the operator's global setting")
	var collation string
	require.NoError(t, db.QueryRow("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='ai_team_session' AND COLUMN_NAME='idempotency_key'", dbName).Scan(&collation))
	assert.Equal(t, "utf8mb4_bin", collation)

	_, err = migrate.Exec(db, "mysql", source, migrate.Down)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow("SELECT value FROM system_setting WHERE category='thread' AND key_name='auto_archive_enabled'").Scan(&value))
	assert.Equal(t, "1", value, "rollback must leave the operator's setting unchanged")
}

func TestAITeamQueriesSurviveProductionCollationShape(t *testing.T) {
	dbName := "octo_ai_team_collation_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
	admin, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1)/?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`") })

	dsn := "root:demo@tcp(127.0.0.1)/" + dbName + "?charset=utf8mb4&parseTime=true&multiStatements=true"
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Production's dump-imported identity tables inherit 0900_ai_ci, while
	// thread and the new AI tables explicitly use general_ci. Keep this drift
	// deliberate: a same-collation CI database cannot catch MySQL error 1267.
	for _, ddl := range []string{
		`CREATE TABLE system_setting (category VARCHAR(64), key_name VARCHAR(128), value TEXT, value_type VARCHAR(16), description VARCHAR(255), UNIQUE KEY uk_category_key(category,key_name)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE robot (robot_id VARCHAR(40) PRIMARY KEY, creator_uid VARCHAR(40), status TINYINT, agent_hosting VARCHAR(64) NOT NULL DEFAULT '', KEY idx_robot_creator(creator_uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE user (uid VARCHAR(40) PRIMARY KEY, name VARCHAR(100), status TINYINT, is_destroy TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE space (space_id VARCHAR(40) PRIMARY KEY, status TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE space_member (space_id VARCHAR(40), uid VARCHAR(40), status TINYINT, UNIQUE KEY uk_space_member(space_id,uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		"CREATE TABLE `group` (group_no VARCHAR(40) PRIMARY KEY, name VARCHAR(50) NOT NULL DEFAULT '', creator VARCHAR(40), space_id VARCHAR(40), project_id VARCHAR(40) NOT NULL DEFAULT '', purpose VARCHAR(32), status TINYINT, version BIGINT NOT NULL DEFAULT 0, allow_view_history_msg TINYINT NOT NULL DEFAULT 1, allow_external TINYINT NOT NULL DEFAULT 0, allow_no_mention TINYINT NOT NULL DEFAULT 1, KEY idx_group_purpose(purpose)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
		`CREATE TABLE group_member (group_no VARCHAR(40), uid VARCHAR(40), remark VARCHAR(100) NOT NULL DEFAULT '', role TINYINT, version BIGINT, status TINYINT, vercode VARCHAR(80), is_deleted TINYINT NOT NULL DEFAULT 0, invite_uid VARCHAR(40), robot TINYINT, bot_admin TINYINT NOT NULL DEFAULT 0, forbidden_expir_time BIGINT NOT NULL DEFAULT 0, is_external TINYINT NOT NULL DEFAULT 0, source_space_id VARCHAR(40) NOT NULL DEFAULT '', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, UNIQUE KEY uk_group_member(group_no,uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE thread (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, short_id VARCHAR(32), group_no VARCHAR(40), name VARCHAR(100), status TINYINT, message_count BIGINT DEFAULT 0, last_message_content TEXT, last_message_at TIMESTAMP NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, UNIQUE KEY uk_thread_short(short_id), KEY idx_thread_group(group_no)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		`CREATE TABLE thread_setting (group_no VARCHAR(40), short_id VARCHAR(32), uid VARCHAR(40), mute TINYINT NOT NULL DEFAULT 0, UNIQUE KEY uk_thread_setting(group_no,short_id,uid)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
		`CREATE TABLE user_pinned_channel (uid VARCHAR(40), space_id VARCHAR(40), channel_id VARCHAR(80), channel_type TINYINT, UNIQUE KEY uk_pinned(uid,space_id,channel_id,channel_type)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
	} {
		_, err = db.Exec(ddl)
		require.NoError(t, err)
	}
	_, err = migrate.Exec(db, "mysql", &migrate.FileMigrationSource{Dir: "sql"}, migrate.Up)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO user(uid,name,status,is_destroy) VALUES ('human','Human',1,0),('bot_bot','Bot',1,0),('bot2_bot','Bot 2',1,0);
		INSERT INTO robot(robot_id,creator_uid,status) VALUES ('bot_bot','human',1),('bot2_bot','human',1);
		INSERT INTO space(space_id,status) VALUES ('space',1);
		INSERT INTO space_member(space_id,uid,status) VALUES ('space','human',1),('space','bot_bot',1),('space','bot2_bot',1);
		INSERT INTO ` + "`group`" + `(group_no,name,creator,space_id,purpose,status) VALUES ('parent','Bot AI','human','space','ai_session_container',1),('team','我的AI团队','human','space','ai_team_group',1);
		INSERT INTO group_member(group_no,uid,role,version,status,vercode,invite_uid,robot) VALUES ('parent','human',1,1,1,'human@1','human',0),('parent','bot_bot',0,2,1,'bot@1','human',1),('team','human',1,3,1,'human@2','human',0),('team','bot_bot',0,4,1,'bot@2','human',1);
		INSERT INTO thread(short_id,group_no,name,status,last_message_content) VALUES ('session','parent','Session',1,'');
		INSERT INTO ai_team_agent(space_id,user_uid,bot_id,group_no,is_added,container_state) VALUES ('space','human','bot_bot','parent',1,2);
		INSERT INTO ai_team_group(space_id,user_uid,group_no,state) VALUES ('space','human','team',2);
		INSERT INTO ai_team_session(agent_id,short_id,idempotency_key,request_hash,state) SELECT id,'session','key','hash',2 FROM ai_team_agent;`)
	require.NoError(t, err)

	conn, err := dbr.Open("mysql", dsn, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	target, err := aiteampkg.LookupReadySessionTarget(conn.NewSession(nil), "parent____session", "human")
	require.NoError(t, err)
	require.NotNil(t, target)
	assert.Equal(t, "bot_bot", target.BotID)

	// Exercise the shipped service query families rather than a test-owned copy.
	cfg := config.New()
	cfg.DB.MySQLAddr = dsn
	cfg.DB.Migration = false
	ctx := config.NewContext(cfg)
	t.Cleanup(func() { _ = ctx.DB().Close() })
	svc := aiteammod.NewService(ctx)
	agent, err := svc.AddAgent("space", "human", "bot_bot")
	require.NoError(t, err)
	assert.Equal(t, "parent", agent.GroupNo)
	secondAgent, err := svc.AddAgent("space", "human", "bot2_bot")
	require.NoError(t, err, "authorization must compare Bot identities as a set, independent of MySQL collation order")
	require.NotEmpty(t, secondAgent.GroupNo)
	agents, err := svc.ListAgents("space", "human", 0, 20)
	require.NoError(t, err)
	require.Len(t, agents.Groups, 3)
	require.Len(t, agents.Groups[1].Items, 2)
	gotSession, err := svc.GetSession("space", "human", "session")
	require.NoError(t, err)
	assert.Equal(t, "parent____session", gotSession.ChannelID)

	// The pin belongs on the new AI-table operand. With indexed legacy tables,
	// that keeps every identity/Space lookup on const/ref access instead of a
	// deployment-sized full scan on the synchronous message path.
	var plan string
	require.NoError(t, db.QueryRow(`EXPLAIN FORMAT=JSON SELECT a.id
		FROM ai_team_agent a
		JOIN ai_team_session s ON s.agent_id=a.id AND s.state=2
		JOIN thread t ON t.short_id=s.short_id AND t.group_no=a.group_no AND t.status<>3
		JOIN `+"`group`"+` g ON g.group_no=a.group_no COLLATE utf8mb4_0900_ai_ci AND g.purpose='ai_session_container' AND g.status=1
		JOIN robot r ON r.robot_id=a.bot_id COLLATE utf8mb4_0900_ai_ci AND r.status=1 AND r.creator_uid=a.user_uid COLLATE utf8mb4_0900_ai_ci
		JOIN user human_u ON human_u.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_u.status=1 AND human_u.is_destroy<>2
		JOIN user bot_u ON bot_u.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_u.status=1 AND bot_u.is_destroy<>2
		JOIN space sp ON sp.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND sp.status=1
		JOIN space_member human_sm ON human_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND human_sm.uid=a.user_uid COLLATE utf8mb4_0900_ai_ci AND human_sm.status=1
		JOIN space_member bot_sm ON bot_sm.space_id=a.space_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.uid=a.bot_id COLLATE utf8mb4_0900_ai_ci AND bot_sm.status=1
		WHERE a.group_no='parent' AND s.short_id='session' AND a.user_uid='human'`).Scan(&plan))
	assert.NotContains(t, plan, `"access_type": "ALL"`, plan)
}

func TestAITeamHandlersUseLocalizedErrors(t *testing.T) {
	for _, file := range []string{"api.go", "api_i18n.go"} {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		source := string(data)
		for _, forbidden := range []string{"ResponseError(", "ResponseErrorf(", "ResponseErrorL(c,", "AbortWithStatusJSON(", ".JSON("} {
			assert.False(t, strings.Contains(source, forbidden), "%s: legacy/raw response call %q is forbidden", file, forbidden)
		}
	}
}
