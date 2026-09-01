//go:build integration

package message

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

// TestIntegration_SetManualUnreadSkipsConversationSync verifies that setting
// the application-level marker neither queries nor depends on WuKongIM's real
// unread state. The fake target deliberately has normal unread messages; the
// marker must still be created without calling /conversation/sync.
func TestIntegration_SetManualUnreadSkipsConversationSync(t *testing.T) {
	channelID := "set-manual-unread-without-sync"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Unread:      7,
			LastMsgSeq:  42,
		},
	}

	s, _ := setupConvSyncE2E(t, fakeIMConvs)

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
	require.Zero(t, fakeIMSyncCalls, "setManualUnread 不应查询 WuKongIM 会话或真实未读数")

	resp = call()
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
	require.True(t, result.ManualUnread)
	require.False(t, result.Changed)
	require.Equal(t, "already_manual_unread", result.Reason)
	require.Zero(t, fakeIMSyncCalls, "幂等设置同样不应查询 WuKongIM")
}

// TestIntegration_ClearManualUnreadAndClearUnreadAlwaysClearManualState
// verifies the two explicit ways a manual marker is cleared:
//   - clearManualUnread changes only conversation_extra.manual_unread;
//   - clearUnread also clears manual_unread when unread is non-zero, because
//     the existing frontend uses that endpoint for explicit unread updates.
func TestIntegration_ClearManualUnreadAndClearUnreadAlwaysClearManualState(t *testing.T) {
	channelID := "clear-manual-unread-group____topic"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
			Timestamp:   1_700_000_000,
			LastMsgSeq:  100,
			Unread:      11,
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

	// Re-create the manual marker, then use the existing clearUnread endpoint
	// with unread>0. That explicit real-unread update must clear the marker too.
	setResp = call(
		"/v1/conversation/setManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":5}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	clearResp := call(
		"/v1/conversation/clearUnread",
		`{"channel_id":"`+channelID+`","channel_type":5,"unread":5}`,
	)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	require.Contains(t, fakeIMCMDs, common.CMDSyncConversationExtra,
		"clearUnread 清除手动未读后必须通知客户端同步会话扩展")
	require.Contains(t, fakeIMCMDs, common.CMDConversationUnreadClear,
		"clearUnread 仍然必须发送真实未读清除命令")

	extra, err = extraDB.queryOne(testutil.UID, channelID, common.ChannelTypeCommunityTopic.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)
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
	require.NoError(t, extraDB.setManualUnread(
		testutil.UID,
		channelID,
		common.ChannelTypeGroup.Uint8(),
		version,
	))

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
	require.NoError(t, extraDB.setManualUnread(
		testutil.UID,
		channelID,
		common.ChannelTypePerson.Uint8(),
		version,
	))

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
		require.NoError(t, extraDB.setManualUnread(testutil.UID, tc.channelID, tc.channelType, version))
	}

	rows, err := extraDB.queryManualUnread(testutil.UID)
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
