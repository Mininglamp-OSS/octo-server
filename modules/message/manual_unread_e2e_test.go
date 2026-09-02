package message

// These tests intentionally run in the default modules/message E2E lane. That
// lane provisions MySQL, Redis, and WuKongIM and executes the package with
// -race and -shuffle; keeping the manual-unread correctness tests there makes
// their concurrency, database, and notification assertions visible to CI.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func requireManualUnreadCMDParam(
	t *testing.T,
	record fakeIMCMDRecord,
	channelID string,
	channelType uint8,
	manualUnread bool,
	version int64,
) {
	t.Helper()
	require.Equal(t, common.CMDSyncConversationExtra, record.CMD)
	require.Equal(t, channelID, record.Param["channel_id"])
	require.Equal(t, float64(channelType), record.Param["channel_type"])
	require.Equal(t, manualUnread, record.Param["manual_unread"])
	require.Equal(t, float64(version), record.Param["version"])
}

// TestIntegration_SetManualUnreadDoesNotQueryIMConversation verifies that the
// dedicated marker is independent of WuKongIM conversation existence and real
// unread state. Even if the fake IM would report unread messages, the endpoint
// must not call /conversations before persisting the user's marker.
func TestIntegration_SetManualUnreadDoesNotQueryIMConversation(t *testing.T) {
	channelID := "set-manual-unread-without-im-query"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Unread:      7,
			LastMsgSeq:  42,
		},
	}

	s, ctx := setupConvSyncE2E(t, fakeIMConvs)

	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		body := bytes.NewBufferString(`{"channel_id":"` + channelID + `","channel_type":2}`)
		req := httptest.NewRequest(http.MethodPut, "/v1/conversation/setManualUnread", body)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	resp := call()
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var result setManualUnreadResp
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
	require.Equal(t, channelID, result.ChannelID)
	require.Equal(t, uint8(common.ChannelTypeGroup), result.ChannelType)
	require.True(t, result.ManualUnread)
	require.True(t, result.Changed)
	require.NotZero(t, result.Version)
	require.Empty(t, result.Reason)
	require.Zero(t, fakeIMConversationCalls, "setManualUnread 不得查询 WuKongIM Conversation")
	require.Len(t, fakeIMCMDRecords, 1)
	requireManualUnreadCMDParam(
		t,
		fakeIMCMDRecords[0],
		channelID,
		common.ChannelTypeGroup.Uint8(),
		true,
		result.Version,
	)
	extra, err := newConversationExtraDB(ctx).queryOne(
		testutil.UID,
		channelID,
		common.ChannelTypeGroup.Uint8(),
	)
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread)
	require.Equal(t, result.Version, extra.Version)

	resp = call()
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
	require.True(t, result.ManualUnread)
	require.False(t, result.Changed)
	require.Equal(t, "already_manual_unread", result.Reason)
	require.Zero(t, fakeIMConversationCalls, "幂等设置也不得查询 WuKongIM Conversation")
	require.Len(t, fakeIMCMDRecords, 1, "幂等设置不应重复通知")
}

// TestIntegration_ClearManualUnreadAndClearUnreadZeroClearManualState verifies
// the two explicit ways a manual marker is cleared:
//   - clearManualUnread changes only conversation_extra.manual_unread;
//   - clearUnread clears it only when the requested real unread count is zero.
func TestIntegration_ClearManualUnreadAndClearUnreadZeroClearManualState(t *testing.T) {
	channelID := "clear-manual-unread-group____topic"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Timestamp:   1_700_000_000,
			LastMsgSeq:  100,
			Unread:      0,
		},
	}

	s, ctx := setupConvSyncE2E(t, fakeIMConvs)

	call := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(body))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	setResp := call(
		"/v1/conversation/setManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	clearManualResp := call(
		"/v1/conversation/clearManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, clearManualResp.Code, clearManualResp.Body.String())
	var clearResult clearManualUnreadResp
	require.NoError(t, json.Unmarshal(clearManualResp.Body.Bytes(), &clearResult))
	require.False(t, clearResult.ManualUnread)
	require.True(t, clearResult.Changed)
	require.NotZero(t, clearResult.Version)

	// Clearing an already-clear marker is a successful no-op. In particular,
	// it must not create another conversation-extra version or notify clients.
	clearAgainResp := call(
		"/v1/conversation/clearManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, clearAgainResp.Code, clearAgainResp.Body.String())
	var clearAgainResult clearManualUnreadResp
	require.NoError(t, json.Unmarshal(clearAgainResp.Body.Bytes(), &clearAgainResult))
	require.False(t, clearAgainResult.ManualUnread)
	require.False(t, clearAgainResult.Changed)
	require.Zero(t, clearAgainResult.Version)

	extraDB := newConversationExtraDB(ctx)
	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)

	// Re-create the manual marker, then retain a positive real unread count.
	// This must not clear the explicit manual marker.
	setResp = call(
		"/v1/conversation/setManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	fakeIMCMDs = nil
	fakeIMCMDRecords = nil
	clearResp := call(
		"/v1/conversation/clearUnread",
		`{"channel_id":"`+channelID+`","channel_type":5,"unread":5}`,
	)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	require.Contains(t, fakeIMCMDs, common.CMDConversationUnreadClear,
		"clearUnread 仍然必须发送真实未读清除命令")
	require.NotContains(t, fakeIMCMDs, common.CMDSyncConversationExtra,
		"unread>0 时不得清除或同步手动未读状态")

	extra, err = extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread)

	// unread=0 is the only clearUnread mode that also clears the marker.
	fakeIMCMDs = nil
	fakeIMCMDRecords = nil
	clearResp = call(
		"/v1/conversation/clearUnread",
		`{"channel_id":"`+channelID+`","channel_type":5,"unread":0}`,
	)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	require.Equal(t, []string{
		common.CMDConversationUnreadClear,
		common.CMDSyncConversationExtra,
	}, fakeIMCMDs)
	extra, err = extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)
	require.Len(t, fakeIMCMDRecords, 2)
	requireManualUnreadCMDParam(
		t,
		fakeIMCMDRecords[1],
		channelID,
		common.ChannelTypeCommunityTopic.Uint8(),
		false,
		extra.Version,
	)
}

// TestIntegration_ClearUnreadBroadcastsCoreStateBeforeBestEffortManualSync
// guards the legacy clearUnread contract. Once WuKongIM has accepted the real
// unread clear, CMDConversationUnreadClear must be broadcast before the
// supplementary conversation-extra notification. A failure of that later
// notification is logged but must not turn the completed core operation into
// an error response.
func TestIntegration_ClearUnreadBroadcastsCoreStateBeforeBestEffortManualSync(t *testing.T) {
	const channelID = "clear-unread-core-before-manual-sync"

	s, ctx := setupConvSyncE2E(t, nil)
	call := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(body))
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	setResp := call(
		"/v1/conversation/setManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	fakeIMCMDs = nil
	fakeIMCMDRecords = nil
	fakeIMFailCMD = common.CMDSyncConversationExtra
	defer func() { fakeIMFailCMD = "" }()

	clearResp := call(
		"/v1/conversation/clearUnread",
		`{"channel_id":"`+channelID+`","channel_type":5,"unread":0}`,
	)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	require.Equal(t, []string{
		common.CMDConversationUnreadClear,
		common.CMDSyncConversationExtra,
	}, fakeIMCMDs, "核心真实未读通知必须先于 best-effort 会话扩展通知")

	extra, err := newConversationExtraDB(ctx).queryOne(
		testutil.UID,
		channelID,
		common.ChannelTypeCommunityTopic.Uint8(),
	)
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)
	require.Len(t, fakeIMCMDRecords, 2)
	requireManualUnreadCMDParam(
		t,
		fakeIMCMDRecords[1],
		channelID,
		common.ChannelTypeCommunityTopic.Uint8(),
		false,
		extra.Version,
	)
}

// TestIntegration_DedicatedManualUnreadNotificationsAreBestEffort verifies
// that both dedicated marker endpoints return their committed state even when
// the transient conversation-extra notification cannot be delivered. A CMD
// failure must not produce a retryable HTTP error after the database commit.
func TestIntegration_DedicatedManualUnreadNotificationsAreBestEffort(t *testing.T) {
	const channelID = "manual-unread-best-effort-notification"

	s, ctx := setupConvSyncE2E(t, nil)
	fakeIMFailCMD = common.CMDSyncConversationExtra
	defer func() { fakeIMFailCMD = "" }()

	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodPut,
			path,
			bytes.NewBufferString(`{"channel_id":"`+channelID+`","channel_type":2}`),
		)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(resp, req)
		return resp
	}

	setResp := call("/v1/conversation/setManualUnread")
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())
	var setResult setManualUnreadResp
	require.NoError(t, json.Unmarshal(setResp.Body.Bytes(), &setResult))
	require.True(t, setResult.Changed)
	require.True(t, setResult.ManualUnread)
	require.NotZero(t, setResult.Version)

	extraDB := newConversationExtraDB(ctx)
	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeGroup.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread)

	clearResp := call("/v1/conversation/clearManualUnread")
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	var clearResult clearManualUnreadResp
	require.NoError(t, json.Unmarshal(clearResp.Body.Bytes(), &clearResult))
	require.True(t, clearResult.Changed)
	require.False(t, clearResult.ManualUnread)
	require.NotZero(t, clearResult.Version)

	extra, err = extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeGroup.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)
	require.Equal(t, []string{
		common.CMDSyncConversationExtra,
		common.CMDSyncConversationExtra,
	}, fakeIMCMDs)
	require.Len(t, fakeIMCMDRecords, 2)
	requireManualUnreadCMDParam(
		t,
		fakeIMCMDRecords[0],
		channelID,
		common.ChannelTypeGroup.Uint8(),
		true,
		setResult.Version,
	)
	requireManualUnreadCMDParam(
		t,
		fakeIMCMDRecords[1],
		channelID,
		common.ChannelTypeGroup.Uint8(),
		false,
		clearResult.Version,
	)
}

func TestIntegration_ConcurrentSetManualUnreadHasOneStateTransition(t *testing.T) {
	const channelID = "manual-unread-concurrent-set"

	s, ctx := setupConvSyncE2E(t, nil)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	call := func() {
		<-start
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodPut,
			"/v1/conversation/setManualUnread",
			bytes.NewBufferString(`{"channel_id":"`+channelID+`","channel_type":2}`),
		)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(resp, req)
		responses <- resp
	}

	go call()
	go call()
	close(start)

	changedCount := 0
	unchangedCount := 0
	var changedVersion int64
	for range 2 {
		resp := <-responses
		require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
		var result setManualUnreadResp
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
		if result.Changed {
			changedCount++
			changedVersion = result.Version
			continue
		}
		unchangedCount++
		require.Equal(t, "already_manual_unread", result.Reason)
		require.Zero(t, result.Version)
	}

	require.Equal(t, 1, changedCount)
	require.Equal(t, 1, unchangedCount)
	require.NotZero(t, changedVersion)
	extra, err := newConversationExtraDB(ctx).queryOne(
		testutil.UID,
		channelID,
		common.ChannelTypeGroup.Uint8(),
	)
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread)
	require.Equal(t, changedVersion, extra.Version)
}

func TestIntegration_ConversationExtraDBRejectsVersionRollback(t *testing.T) {
	const channelID = "manual-unread-version-guard"

	_, ctx := setupConvSyncE2E(t, nil)
	extraDB := newConversationExtraDB(ctx)
	channelType := common.ChannelTypeGroup.Uint8()

	changed, err := extraDB.setManualUnread(testutil.UID, channelID, channelType, 200)
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = extraDB.setManualUnread(testutil.UID, channelID, channelType, 100)
	require.NoError(t, err)
	require.False(t, changed, "a stale set must not overwrite a newer version")

	changed, err = extraDB.clearManualUnread(testutil.UID, channelID, channelType, 150)
	require.NoError(t, err)
	require.False(t, changed, "a stale clear must not overwrite a newer version")

	changed, err = extraDB.insertOrUpdate(&conversationExtraModel{
		UID:            testutil.UID,
		ChannelID:      channelID,
		ChannelType:    channelType,
		BrowseTo:       9,
		KeepMessageSeq: 9,
		Draft:          "stale",
		Version:        180,
	})
	require.NoError(t, err)
	require.False(t, changed, "a stale ordinary extra update must be rejected")

	extra, err := extraDB.queryOne(testutil.UID, channelID, channelType)
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread)
	require.Equal(t, int64(200), extra.Version)
	require.Empty(t, extra.Draft)

	changed, err = extraDB.insertOrUpdate(&conversationExtraModel{
		UID:            testutil.UID,
		ChannelID:      channelID,
		ChannelType:    channelType,
		BrowseTo:       9,
		KeepMessageSeq: 9,
		Draft:          "fresh",
		Version:        201,
	})
	require.NoError(t, err)
	require.True(t, changed)

	changed, err = extraDB.clearManualUnread(testutil.UID, channelID, channelType, 202)
	require.NoError(t, err)
	require.True(t, changed)
	extra, err = extraDB.queryOne(testutil.UID, channelID, channelType)
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)
	require.Equal(t, int64(202), extra.Version)
	require.Equal(t, "fresh", extra.Draft)
}

func TestIntegration_SetManualUnreadAdvancesPastPersistedUIDHighWater(t *testing.T) {
	const (
		seedChannelID = "manual-unread-high-water-seed"
		targetChannel = "manual-unread-high-water-target"
		highWater     = int64(9_000_000_000_000)
	)

	s, ctx := setupConvSyncE2E(t, nil)
	extraDB := newConversationExtraDB(ctx)
	changed, err := extraDB.setManualUnread(
		testutil.UID,
		seedChannelID,
		common.ChannelTypeGroup.Uint8(),
		highWater,
	)
	require.NoError(t, err)
	require.True(t, changed)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/v1/conversation/setManualUnread",
		bytes.NewBufferString(`{"channel_id":"`+targetChannel+`","channel_type":2}`),
	)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

	var result setManualUnreadResp
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
	require.True(t, result.Changed)
	require.Equal(t, highWater+1, result.Version)

	extra, err := extraDB.queryOne(testutil.UID, targetChannel, common.ChannelTypeGroup.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.Equal(t, highWater+1, extra.Version)
}

// TestIntegration_ConversationExtraUpdatePreservesOmittedManualUnread verifies
// that a legacy extra update, which does not include manual_unread, cannot
// overwrite a marker set by the dedicated manual-unread endpoint.
func TestIntegration_ConversationExtraUpdatePreservesOmittedManualUnread(t *testing.T) {
	channelID := "conversation-extra-preserve-manual-unread"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Timestamp:   1_700_000_000,
			LastMsgSeq:  100,
			Unread:      11,
		},
	}

	s, ctx := setupConvSyncE2E(t, fakeIMConvs)
	extraDB := newConversationExtraDB(ctx)
	version, err := ctx.GenSeq(common.SyncConversationExtraKey)
	require.NoError(t, err)
	changed, err := extraDB.setManualUnread(
		testutil.UID,
		channelID,
		common.ChannelTypeGroup.Uint8(),
		version,
	)
	require.NoError(t, err)
	require.True(t, changed)

	body := bytes.NewBufferString(`{"browse_to":11,"keep_message_seq":11,"keep_offset_y":0,"draft":"draft"}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/conversations/"+channelID+"/2/extra",
		body,
	)
	req.Header.Set("token", testutil.Token)
	resp := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeGroup.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread,
		"未携带 manual_unread 的旧扩展更新必须保留数据库中的手动未读状态")
}

// TestIntegration_ManualUnreadRejectsPerson verifies the temporary product
// boundary: manual-unread is supported only for groups and community topics.
// A stale Person marker must neither make the endpoints accept the request nor
// be mutated as a side effect of the rejected request.
func TestIntegration_ManualUnreadRejectsPerson(t *testing.T) {
	const channelID = "manual-unread-person-unsupported"

	s, ctx := setupConvSyncE2E(t, nil)
	extraDB := newConversationExtraDB(ctx)
	version, err := ctx.GenSeq(common.SyncConversationExtraKey)
	require.NoError(t, err)
	changed, err := extraDB.setManualUnread(
		testutil.UID,
		channelID,
		common.ChannelTypePerson.Uint8(),
		version,
	)
	require.NoError(t, err)
	require.True(t, changed)

	for _, path := range []string{
		"/v1/conversation/setManualUnread",
		"/v1/conversation/clearManualUnread",
	} {
		t.Run(path, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequest(
				http.MethodPut,
				path,
				bytes.NewBufferString(`{"channel_id":"`+channelID+`","channel_type":1}`),
			)
			req.Header.Set("token", testutil.Token)
			s.GetRoute().ServeHTTP(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
			var envelope struct {
				Status int `json:"status"`
			}
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
			require.Equal(t, http.StatusBadRequest, envelope.Status)
		})
	}

	clearResp := httptest.NewRecorder()
	clearReq := httptest.NewRequest(
		http.MethodPut,
		"/v1/conversation/clearUnread",
		bytes.NewBufferString(`{"channel_id":"`+channelID+`","channel_type":1,"unread":0}`),
	)
	clearReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(clearResp, clearReq)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())

	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypePerson.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread, "私聊专用请求和 clearUnread 都不能维护遗留手动未读状态")
	require.NotContains(t, fakeIMCMDs, common.CMDSyncConversationExtra)
	require.Contains(t, fakeIMCMDs, common.CMDConversationUnreadClear)
	require.Zero(t, fakeIMSyncCalls)
}

func TestIntegration_QueryManualUnreadExcludesPerson(t *testing.T) {
	_, ctx := setupConvSyncE2E(t, nil)
	extraDB := newConversationExtraDB(ctx)

	for _, tc := range []struct {
		channelID   string
		channelType uint8
	}{
		{channelID: "manual-unread-person-hidden", channelType: common.ChannelTypePerson.Uint8()},
		{channelID: "manual-unread-group-visible", channelType: common.ChannelTypeGroup.Uint8()},
		{channelID: "manual-unread-topic-visible", channelType: common.ChannelTypeCommunityTopic.Uint8()},
	} {
		version, err := ctx.GenSeq(common.SyncConversationExtraKey)
		require.NoError(t, err)
		changed, err := extraDB.setManualUnread(testutil.UID, tc.channelID, tc.channelType, version)
		require.NoError(t, err)
		require.True(t, changed)
	}

	rows, err := extraDB.queryManualUnread(
		testutil.UID,
		[]string{"manual-unread-group-visible"},
		[]string{"manual-unread-topic-visible"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.True(t, supportsManualUnreadChannelType(row.ChannelType))
	}
}

func TestIntegration_ManualUnreadSupportsGroupAndCommunityTopic(t *testing.T) {
	s, ctx := setupConvSyncE2E(t, nil)

	for _, tc := range []struct {
		name        string
		channelID   string
		channelType uint8
	}{
		{name: "group", channelID: "manual-unread-group", channelType: common.ChannelTypeGroup.Uint8()},
		{name: "community topic", channelID: "manual-unread-group____topic", channelType: common.ChannelTypeCommunityTopic.Uint8()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := func(path string) *httptest.ResponseRecorder {
				resp := httptest.NewRecorder()
				req := httptest.NewRequest(
					http.MethodPut,
					path,
					bytes.NewBufferString(fmt.Sprintf(
						`{"channel_id":%q,"channel_type":%d}`,
						tc.channelID,
						tc.channelType,
					)),
				)
				req.Header.Set("token", testutil.Token)
				s.GetRoute().ServeHTTP(resp, req)
				return resp
			}

			setResp := call("/v1/conversation/setManualUnread")
			require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())
			extraDB := newConversationExtraDB(ctx)
			extra, err := extraDB.queryOne(testutil.UID, tc.channelID, tc.channelType)
			require.NoError(t, err)
			require.NotNil(t, extra)
			require.True(t, extra.ManualUnread)

			clearResp := call("/v1/conversation/clearManualUnread")
			require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
			extra, err = extraDB.queryOne(testutil.UID, tc.channelID, tc.channelType)
			require.NoError(t, err)
			require.NotNil(t, extra)
			require.False(t, extra.ManualUnread)
		})
	}
}
