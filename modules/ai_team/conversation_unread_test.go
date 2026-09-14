package ai_team_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	ai "github.com/Mininglamp-OSS/octo-server/modules/ai_team"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createUnreadSession(t *testing.T, f fixture, name string) ai.Session {
	t.Helper()
	w := request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), `"unread"`, "AI metadata API retains its original contract")
	w = request(t, f, http.MethodPost, "/v1/ai-team/agents/"+f.botID+"/sessions", name, map[string]string{"name": name})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), `"unread"`)
	var session ai.Session
	decodeJSON(t, w, &session)
	return session
}

func useUnreadIM(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	stub := httptest.NewServer(handler)
	cfg := testContext.GetConfig()
	oldURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.URL
	t.Cleanup(func() { cfg.WuKongIM.APIURL = oldURL; stub.Close() })
}

func unreadConversation(channelID string, channelType uint8, uid string, unread int) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID: channelID, ChannelType: channelType, Unread: unread, Version: 42, LastMsgSeq: 12, Timestamp: time.Now().Unix(),
		Recents: []*config.MessageResp{{ChannelID: channelID, ChannelType: channelType, FromUID: uid, MessageID: 123, MessageSeq: 12, Timestamp: int32(time.Now().Unix()), Payload: []byte(`{"type":1,"content":"Reply"}`)}},
	}
}

func syncUnread(t *testing.T, f fixture, body map[string]any) (message.SyncUserConversationRespWrap, map[string]json.RawMessage) {
	t.Helper()
	w := request(t, f, http.MethodPost, "/v1/conversation/sync", "", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp message.SyncUserConversationRespWrap
	var raw map[string]json.RawMessage
	decodeJSON(t, w, &resp)
	decodeJSON(t, w, &raw)
	return resp, raw
}

func TestAITeamConversationUnreadCompatibilityAndIsolation(t *testing.T) {
	f := seedFixture(t)
	first := createUnreadSession(t, f, "first")
	archived := createUnreadSession(t, f, "archived")
	deleted := createUnreadSession(t, f, "deleted")
	failed := createUnreadSession(t, f, "failed")
	_, err := testContext.DB().Update("thread").Set("status", thread.ThreadStatusArchived).Where("short_id=?", archived.ShortID).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO thread_setting (group_no,short_id,uid,mute) VALUES (?,?,?,1)", first.GroupNo, first.ShortID, f.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().Update("thread").Set("status", thread.ThreadStatusDeleted).Where("short_id=?", deleted.ShortID).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().Update("ai_team_session").Set("state", 3).Where("short_id=?", failed.ShortID).Exec()
	require.NoError(t, err)
	other := f
	other.botID += "_other"
	_, err = testContext.DB().InsertBySql("INSERT INTO user (uid,name,short_no,status,is_destroy) VALUES (?,?,?,1,0)", other.botID, "Other", other.botID).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO robot (robot_id,creator_uid,status) VALUES (?,?,1)", other.botID, f.uid).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", f.spaceID, other.botID).Exec()
	require.NoError(t, err)
	otherSession := createUnreadSession(t, other, "other")
	foreignSpace := f
	foreignSpace.spaceID += "_foreign"
	_, err = testContext.DB().InsertBySql("INSERT INTO space (space_id,name,creator,status) VALUES (?,?,?,1)", foreignSpace.spaceID, "Foreign", f.uid).Exec()
	require.NoError(t, err)
	for _, uid := range []string{f.uid, f.botID} {
		_, err = testContext.DB().InsertBySql("INSERT INTO space_member (space_id,uid,status) VALUES (?,?,1)", foreignSpace.spaceID, uid).Exec()
		require.NoError(t, err)
	}
	foreign := createUnreadSession(t, foreignSpace, "foreign")
	// Seed the unrelated aggregate purpose directly: unread sync must not depend
	// on the separate, unmerged AI Team aggregate-group feature.
	teamGroup := f.uid + "_team"
	_, err = testContext.DB().InsertBySql("INSERT INTO `group` (group_no,name,creator,space_id,purpose,status) VALUES (?,?,?,?,?,1)", teamGroup, "Team", f.uid, f.spaceID, "ai_team_group").Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO group_member (group_no,uid,status,is_deleted,role,version,vercode) VALUES (?,?,1,0,1,1,?)", teamGroup, f.uid, teamGroup+"@1").Exec()
	require.NoError(t, err)
	// An ordinary group gives the compatibility comparison non-empty real chat data.
	normal := f.uid + "_normal"
	_, err = testContext.DB().InsertBySql("INSERT INTO `group` (group_no,name,creator,space_id,status) VALUES (?,?,?,?,1)", normal, "Normal", f.uid, f.spaceID).Exec()
	require.NoError(t, err)
	_, err = testContext.DB().InsertBySql("INSERT INTO group_member (group_no,uid,status,is_deleted,role,version,vercode) VALUES (?,?,1,0,1,1,?)", normal, f.uid, normal+"@1").Exec()
	require.NoError(t, err)
	conv := func(id string, typ uint8, n int) *config.SyncUserConversationResp {
		return unreadConversation(id, typ, f.botID, n)
	}
	items := []*config.SyncUserConversationResp{
		conv(normal, 2, 9), conv(first.ChannelID, 5, 3), conv(archived.ChannelID, 5, 5), conv(otherSession.ChannelID, 5, 7),
		conv(deleted.ChannelID, 5, 101), conv(failed.ChannelID, 5, 102), conv(foreign.ChannelID, 5, 103),
		conv(first.GroupNo, 2, 104), conv(teamGroup, 2, 105), conv(first.ChannelID, 2, 106),
		conv(first.GroupNo+"____unregistered", 5, 107), conv(otherSession.GroupNo+"____"+first.ShortID, 5, 108),
	}
	var calls atomic.Int32
	var requests []map[string]any
	useUnreadIM(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NotEqual(t, "/conversation/syncByChannels", r.URL.Path)
		if r.URL.Path != "/conversation/sync" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		requests = append(requests, payload)
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(items)
	})
	body := map[string]any{"msg_count": 1, "recent_filter": true}
	t.Setenv("DM_AI_TEAM_ON", "false")
	before, original := syncUnread(t, f, body)
	assert.Nil(t, before.AITeamConversations)
	t.Setenv("DM_AI_TEAM_ON", "true")
	after, extended := syncUnread(t, f, body)
	assert.EqualValues(t, 2, calls.Load(), "one IM request for each existing sync")
	require.Len(t, requests, 2)
	assert.Equal(t, requests[0], requests[1], "upstream sync parameters are unchanged")
	delete(extended, "ai_team_conversations")
	assert.Equal(t, original, extended, "all original response fields retain the same values")
	require.NotNil(t, after.AITeamConversations)
	assert.Equal(t, "full", after.AITeamConversations.Mode)
	require.Len(t, after.AITeamConversations.Items, 3)
	totals := map[string]int64{}
	for _, item := range after.AITeamConversations.Items {
		assert.Equal(t, f.spaceID, item.SpaceID)
		assert.EqualValues(t, 5, item.ChannelType)
		totals[item.BotID] += item.Unread
	}
	assert.EqualValues(t, 8, totals[f.botID], "client aggregates the agent's sessions, never either parent group")
	assert.EqualValues(t, 7, totals[other.botID])
	foundNormal := false
	for _, item := range after.Conversations {
		if item.ChannelID == normal {
			foundNormal = true
			assert.Equal(t, 9, item.Unread)
		}
	}
	assert.True(t, foundNormal)
	// Revoked owner authority must also remove previously valid raw IM entries.
	_, err = testContext.DB().Update("robot").Set("creator_uid", "another-owner").Where("robot_id=?", f.botID).Exec()
	require.NoError(t, err)
	filtered, _ := syncUnread(t, f, body)
	require.NotNil(t, filtered.AITeamConversations)
	require.Len(t, filtered.AITeamConversations.Items, 1)
	assert.Equal(t, other.botID, filtered.AITeamConversations.Items[0].BotID)
	// A blacklisted parent member cannot read AI sideband data.
	_, err = testContext.DB().Update("group_member").Set("status", 2).Where("group_no=? AND uid=?", otherSession.GroupNo, f.uid).Exec()
	require.NoError(t, err)
	filtered, _ = syncUnread(t, f, body)
	require.NotNil(t, filtered.AITeamConversations)
	assert.Empty(t, filtered.AITeamConversations.Items)
}

func TestAITeamConversationUnreadModesAndFailure(t *testing.T) {
	f := seedFixture(t)
	session := createUnreadSession(t, f, "session")
	items := []*config.SyncUserConversationResp{unreadConversation(session.ChannelID, 5, f.botID, 0)}
	var calls atomic.Int32
	useUnreadIM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversation/sync" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(items)
	})
	full, _ := syncUnread(t, f, map[string]any{"msg_count": 1})
	require.NotNil(t, full.AITeamConversations)
	assert.Equal(t, "full", full.AITeamConversations.Mode)
	require.Len(t, full.AITeamConversations.Items, 1)
	assert.Zero(t, full.AITeamConversations.Items[0].Unread)
	encoded, err := json.Marshal(full.AITeamConversations.Items[0])
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"unread":0`)
	for _, body := range []map[string]any{{"msg_count": 1, "version": 1}, {"msg_count": 1, "last_msg_seqs": session.ChannelID + ":5:12"}} {
		delta, _ := syncUnread(t, f, body)
		require.NotNil(t, delta.AITeamConversations)
		assert.Equal(t, "delta", delta.AITeamConversations.Mode)
	}
	zero, _ := syncUnread(t, f, map[string]any{"msg_count": 0})
	assert.Nil(t, zero.AITeamConversations, "no-message sync must not masquerade as an empty full snapshot")
	// Optional mapping failure omits only the new field and preserves ordinary sync.
	_, err = testContext.DB().UpdateBySql("RENAME TABLE ai_team_session TO ai_team_session_unread_test").Exec()
	require.NoError(t, err)
	defer func() {
		_, restoreErr := testContext.DB().UpdateBySql("RENAME TABLE ai_team_session_unread_test TO ai_team_session").Exec()
		require.NoError(t, restoreErr)
	}()
	unavailable, _ := syncUnread(t, f, map[string]any{"msg_count": 1})
	assert.Nil(t, unavailable.AITeamConversations)
	items = nil
	empty, _ := syncUnread(t, f, map[string]any{"msg_count": 1})
	require.NotNil(t, empty.AITeamConversations, "no AI candidates requires no metadata query")
	assert.Equal(t, "full", empty.AITeamConversations.Mode)
	assert.Empty(t, empty.AITeamConversations.Items)
	delta, raw := syncUnread(t, f, map[string]any{"msg_count": 1, "version": 1})
	assert.Equal(t, "delta", delta.AITeamConversations.Mode)
	assert.Empty(t, delta.AITeamConversations.Items)
	assert.Contains(t, string(raw["ai_team_conversations"]), `"items":[]`)
	assert.EqualValues(t, 7, calls.Load())
}

func TestAITeamConversationUnreadUsesExistingIMReadState(t *testing.T) {
	f := seedFixture(t)
	session := createUnreadSession(t, f, "live")
	require.NoError(t, testContext.SendMessage(&config.MsgSendReq{
		Header: config.MsgHeader{RedDot: 1}, FromUID: f.botID, ChannelID: session.ChannelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(), Payload: []byte(`{"type":1,"content":"Reply"}`),
	}))
	require.Eventually(t, func() bool {
		resp, _ := syncUnread(t, f, map[string]any{"msg_count": 1})
		return resp.AITeamConversations != nil && len(resp.AITeamConversations.Items) == 1 && resp.AITeamConversations.Items[0].Unread == 1
	}, 5*time.Second, 100*time.Millisecond)
	w := request(t, f, http.MethodPut, "/v1/conversation/clearUnread", "", map[string]any{"channel_id": session.ChannelID, "channel_type": 5, "unread": 0})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp, _ := syncUnread(t, f, map[string]any{"msg_count": 1})
	require.NotNil(t, resp.AITeamConversations)
	require.Len(t, resp.AITeamConversations.Items, 1)
	assert.Zero(t, resp.AITeamConversations.Items[0].Unread)
	// Session deletion retains the original API behavior; next sync filters it.
	w = request(t, f, http.MethodDelete, "/v1/ai-team/sessions/"+session.ShortID, "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	resp, _ = syncUnread(t, f, map[string]any{"msg_count": 1})
	require.NotNil(t, resp.AITeamConversations)
	assert.Empty(t, resp.AITeamConversations.Items)
}
