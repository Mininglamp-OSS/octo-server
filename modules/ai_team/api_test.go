package ai_team_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	aiteammod "github.com/Mininglamp-OSS/octo-server/modules/ai_team"
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
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", "guard-key", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var created struct {
		GroupNo string `json:"group_no"`
	}
	decodeJSON(t, w, &created)

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

func TestAITeamMigrationDisablesGlobalThreadAutoArchive(t *testing.T) {
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
	assert.Equal(t, "0", value)
	var collation string
	require.NoError(t, db.QueryRow("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME='ai_team_session' AND COLUMN_NAME='idempotency_key'", dbName).Scan(&collation))
	assert.Equal(t, "utf8mb4_bin", collation)

	_, err = migrate.Exec(db, "mysql", source, migrate.Down)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow("SELECT value FROM system_setting WHERE category='thread' AND key_name='auto_archive_enabled'").Scan(&value))
	assert.Equal(t, "1", value, "rollback must restore the operator's prior setting")
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
		`CREATE TABLE robot (robot_id VARCHAR(40), creator_uid VARCHAR(40), status TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE user (uid VARCHAR(40), name VARCHAR(100), status TINYINT, is_destroy TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE space (space_id VARCHAR(40), status TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		`CREATE TABLE space_member (space_id VARCHAR(40), uid VARCHAR(40), status TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		"CREATE TABLE `group` (group_no VARCHAR(40), purpose VARCHAR(32), status TINYINT) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci",
		`CREATE TABLE thread (short_id VARCHAR(32), group_no VARCHAR(40), name VARCHAR(100), status TINYINT, message_count BIGINT DEFAULT 0, last_message_content TEXT, last_message_at TIMESTAMP NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`,
	} {
		_, err = db.Exec(ddl)
		require.NoError(t, err)
	}
	_, err = migrate.Exec(db, "mysql", &migrate.FileMigrationSource{Dir: "sql"}, migrate.Up)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO user(uid,name,status,is_destroy) VALUES ('human','Human',1,0),('bot','Bot',1,0);
		INSERT INTO robot(robot_id,creator_uid,status) VALUES ('bot','human',1);
		INSERT INTO space(space_id,status) VALUES ('space',1);
		INSERT INTO space_member(space_id,uid,status) VALUES ('space','human',1),('space','bot',1);
		INSERT INTO ` + "`group`" + `(group_no,purpose,status) VALUES ('parent','ai_session_container',1);
		INSERT INTO thread(short_id,group_no,name,status) VALUES ('session','parent','Session',1);
		INSERT INTO ai_team_agent(space_id,user_uid,bot_id,group_no,is_added,container_state) VALUES ('space','human','bot','parent',1,2);
		INSERT INTO ai_team_session(agent_id,short_id,idempotency_key,request_hash,state) SELECT id,'session','key','hash',2 FROM ai_team_agent;`)
	require.NoError(t, err)

	conn, err := dbr.Open("mysql", dsn, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	target, err := aiteampkg.LookupReadySessionTarget(conn.NewSession(nil), "parent____session", "human")
	require.NoError(t, err)
	require.NotNil(t, target)
	assert.Equal(t, "bot", target.BotID)

	// Exercise the same collation-pinned join family used by getAgent/ListAgents/GetSession.
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM ai_team_agent a
		JOIN robot r ON r.robot_id COLLATE utf8mb4_general_ci=a.bot_id AND r.creator_uid COLLATE utf8mb4_general_ci=a.user_uid
		JOIN user u ON u.uid COLLATE utf8mb4_general_ci=a.bot_id
		JOIN space sp ON sp.space_id COLLATE utf8mb4_general_ci=a.space_id
		JOIN space_member sm ON sm.space_id COLLATE utf8mb4_general_ci=a.space_id AND sm.uid COLLATE utf8mb4_general_ci=a.user_uid`).Scan(&count))
	assert.Equal(t, 1, count)
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
