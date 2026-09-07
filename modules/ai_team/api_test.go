package ai_team_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	aiteammod "github.com/Mininglamp-OSS/octo-server/modules/ai_team"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
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

	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var first struct {
		BotID   string `json:"bot_id"`
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &first)
	assert.Equal(t, f.botID, first.BotID)
	assert.Empty(t, first.GroupNo, "adding an AI must not eagerly create a group")

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

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
	assert.Equal(t, session1.GroupNo+"____"+session1.SessionID, session1.ChannelID)

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
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
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
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var restored struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &restored)
	assert.Equal(t, session1.GroupNo, restored.GroupNo)
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
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.forbidden")

	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "guard-key", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &created)

	w = request(t, f, http.MethodPost, fmt.Sprintf("/v1/groups/%s/members_delete", created.GroupNo), "", map[string]any{"members": []string{f.botID}})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.ai_team.container_protected")

	var memberCount int
	require.NoError(t, testContext.DB().Select("COUNT(*)").From("group_member").Where("group_no=? AND is_deleted=0", created.GroupNo).LoadOne(&memberCount))
	assert.Equal(t, 2, memberCount)
}

func TestAITeamMigrationDisablesGlobalThreadAutoArchive(t *testing.T) {
	data, err := os.ReadFile("sql/20260907000001_ai_team_sessions.sql")
	require.NoError(t, err)
	source := string(data)
	assert.Contains(t, source, "'thread', 'auto_archive_enabled', '0', 'bool'",
		"migration must persist the global off value")
	assert.Contains(t, source, "ON DUPLICATE KEY UPDATE `value`='0'",
		"migration must override a stale enabled DB setting")
}

func TestAITeamHandlersUseLocalizedErrors(t *testing.T) {
	data, err := os.ReadFile("api.go")
	require.NoError(t, err)
	source := string(data)
	for _, forbidden := range []string{"ResponseError(", "ResponseErrorf(", "AbortWithStatusJSON(", ".JSON("} {
		assert.False(t, strings.Contains(source, forbidden), "legacy/raw response call %q is forbidden", forbidden)
	}
}
