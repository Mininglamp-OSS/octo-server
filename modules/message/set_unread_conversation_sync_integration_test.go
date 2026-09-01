//go:build integration

package message

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// TestIntegration_SetManualUnreadUsesConversationSync verifies the
// regression fixed here: setManualUnread must use WuKongIM's supported
// POST /conversation/sync endpoint instead of the removed GET /conversations
// endpoint. The shared fake IM server returns a valid conversation only for
// the supported sync path and accepts the follow-up no-persist CMD.
func TestIntegration_SetManualUnreadUsesConversationSync(t *testing.T) {
	channelID := "set-manual-unread-sync-group"
	fakeIMConvs = []*config.SyncUserConversationResp{
		{
			ChannelID:   channelID,
			ChannelType: common.ChannelTypeGroup.Uint8(),
			Unread:      0,
			LastMsgSeq:  42,
		},
	}

	s, _ := setupConvSyncE2E(t, fakeIMConvs)

	body := bytes.NewBufferString(`{"channel_id":"` + channelID + `","channel_type":2}`)
	req := httptest.NewRequest(http.MethodPut, "/v1/conversation/setManualUnread", body)
	req.Header.Set("token", testutil.Token)

	resp := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(resp, req)

	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var result setManualUnreadResp
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &result))
	require.Equal(t, channelID, result.ChannelID)
	require.Equal(t, uint8(common.ChannelTypeGroup), result.ChannelType)
	require.True(t, result.ManualUnread)
	require.True(t, result.Changed)
	require.NotZero(t, result.Version)
}

// TestIntegration_ClearManualUnreadAndClearUnreadAlwaysClearManualState
// verifies the two explicit ways a manual marker is cleared:
//   - clearManualUnread changes only conversation_extra.manual_unread;
//   - clearUnread also clears manual_unread when unread is non-zero, because
//     the existing frontend uses that endpoint for explicit unread updates.
func TestIntegration_ClearManualUnreadAndClearUnreadAlwaysClearManualState(t *testing.T) {
	channelID := "clear-manual-unread-dm"
	fakeIMConvs = []*config.SyncUserConversationResp{
		dmIMConv(channelID, 1_700_000_000, 100, 11),
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
		`{"channel_id":"`+channelID+`","channel_type":1}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	clearManualResp := call(
		"/v1/conversation/clearManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":1}`,
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
		`{"channel_id":"`+channelID+`","channel_type":1}`,
	)
	require.Equal(t, http.StatusOK, clearAgainResp.Code, clearAgainResp.Body.String())
	var clearAgainResult clearManualUnreadResp
	require.NoError(t, json.Unmarshal(clearAgainResp.Body.Bytes(), &clearAgainResult))
	require.False(t, clearAgainResult.ManualUnread)
	require.False(t, clearAgainResult.Changed)
	require.Zero(t, clearAgainResult.Version)

	extraDB := newConversationExtraDB(ctx)
	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypePerson.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.False(t, extra.ManualUnread)

	// Re-create the manual marker, then use the existing clearUnread endpoint
	// with unread>0. That explicit real-unread update must clear the marker too.
	setResp = call(
		"/v1/conversation/setManualUnread",
		`{"channel_id":"`+channelID+`","channel_type":1}`,
	)
	require.Equal(t, http.StatusOK, setResp.Code, setResp.Body.String())

	clearResp := call(
		"/v1/conversation/clearUnread",
		`{"channel_id":"`+channelID+`","channel_type":1,"unread":5}`,
	)
	require.Equal(t, http.StatusOK, clearResp.Code, clearResp.Body.String())
	require.Contains(t, fakeIMCMDs, common.CMDSyncConversationExtra,
		"clearUnread 清除手动未读后必须通知客户端同步会话扩展")
	require.Contains(t, fakeIMCMDs, common.CMDConversationUnreadClear,
		"clearUnread 仍然必须发送真实未读清除命令")

	extra, err = extraDB.queryOne(testutil.UID, channelID, common.ChannelTypePerson.Uint8())
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
		dmIMConv(channelID, 1_700_000_000, 100, 11),
	}

	s, ctx := setupConvSyncE2E(t, fakeIMConvs)
	extraDB := newConversationExtraDB(ctx)
	version, err := ctx.GenSeq(common.SyncConversationExtraKey)
	require.NoError(t, err)
	require.NoError(t, extraDB.setManualUnread(
		testutil.UID,
		channelID,
		common.ChannelTypePerson.Uint8(),
		version,
	))

	body := bytes.NewBufferString(`{"browse_to":11,"keep_message_seq":11,"keep_offset_y":0,"draft":"draft"}`)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/conversations/"+channelID+"/1/extra",
		body,
	)
	req.Header.Set("token", testutil.Token)
	resp := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())

	extra, err := extraDB.queryOne(testutil.UID, channelID, common.ChannelTypePerson.Uint8())
	require.NoError(t, err)
	require.NotNil(t, extra)
	require.True(t, extra.ManualUnread,
		"未携带 manual_unread 的旧扩展更新必须保留数据库中的手动未读状态")
}
