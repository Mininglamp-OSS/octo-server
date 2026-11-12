package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/stretchr/testify/require"
)

const (
	cliBotMessageTestToken         = "cli-bot-message-business-token"
	cliBotMessageTestServiceSecret = "cli-bot-message-service-secret-with-at-least-32-bytes"
)

func newCLIBotMessageTarget(t *testing.T, source *wkhttp.WKHttp, ownerID, ownerName, spaceID, botID string, actions []string) *wkhttp.WKHttp {
	t.Helper()
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/internal/tokens/_resolve", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer "+cliBotMessageTestServiceSecret, r.Header.Get("Authorization"))
		var request struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, cliBotMessageTestToken, request.Token)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"principal": map[string]any{
					"token_id": "cli-bot-message-token-id", "owner_id": ownerID,
					"owner_name": ownerName, "space_id": spaceID,
					"subject_type": "user", "subject_id": ownerID,
					"issued_at": "2026-01-01T00:00:00Z",
				},
				"permissions":  []map[string]any{{"domain": "im.messages", "actions": actions}},
				"bot_bindings": []map[string]string{{"bot_id": botID, "bot_name": "Bound Bot"}},
			},
		}))
	}))
	t.Cleanup(authServer.Close)
	target, err := cliv2.NewRouter(authServer.URL, cliBotMessageTestServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(target, func(c *wkhttp.Context) {
		principal := cliv2.PrincipalFrom(c)
		require.Equal(t, ownerID, principal.UserID)
		require.Equal(t, spaceID, principal.SpaceID)
		require.Equal(t, botID, principal.BotID)
		require.Equal(t, botID, c.GetLoginUID())
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, target))
	return target
}

func newCLIBotMessageSource(t *testing.T, ctx *config.Context, ba *BotAPI) *wkhttp.WKHttp {
	t.Helper()
	m := message.New(ctx)
	t.Cleanup(m.Stop)
	source := wkhttp.New()
	file.New(ctx).Route(source)
	m.Route(source)
	ba.Route(source)
	return source
}
func doCLIBotMessageRequest(t *testing.T, target http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+cliBotMessageTestToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	target.ServeHTTP(response, request)
	return response
}

func TestCLIBotMessageSendUsesBoundBotAndRejectsOBO(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const botID = "cli_message_bound_bot"
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, "space_cli_message_bound", ownerID)
	spaceID := "space_cli_message_bound"

	ba := NewBotAPI(ctx)
	ba.spaceQuerier = &fakeSpaceQuerier{defaultSpace: spaceID}
	var sent []*config.MsgSendReq
	ba.dispatchOverride = func(req *config.MsgSendReq) (*config.MsgSendResp, error) {
		copy := *req
		copy.Payload = append([]byte(nil), req.Payload...)
		sent = append(sent, &copy)
		return &config.MsgSendResp{MessageID: 11, MessageSeq: 7, ClientMsgNo: "cli-message-test"}, nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:write"})

	response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+ownerID+`","channel_type":1,"payload":{"type":1,"content":"hello"},"from_uid":"forged-user"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, sent, 1)
	require.Equal(t, botID, sent[0].FromUID)
	var sentPayload map[string]any
	require.NoError(t, json.Unmarshal(sent[0].Payload, &sentPayload))
	require.Equal(t, spaceID, sentPayload["space_id"])
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Contains(t, envelope, "data")

	oboResponse := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+ownerID+`","channel_type":1,"on_behalf_of":"another-user","payload":{"type":1,"content":"hello"}}`)
	require.Equal(t, http.StatusBadRequest, oboResponse.Code, oboResponse.Body.String())
	require.Len(t, sent, 1, "v2 must never dispatch an OBO message")
}

func TestCLIBotMessageChannelsDenyNonMembers(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID   = "cli_message_nonmember_bot"
		spaceID = "space_cli_message_bound"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, 1, ?)",
		"cli_message_denied_group", "Denied group", spaceID,
	).Exec()
	require.NoError(t, err)
	ba := NewBotAPI(ctx)
	ba.groupStatusQueryOverride = func(string) (int, error) { return group.GroupStatusNormal, nil }
	ba.groupMemberCountOverride = func(groupNo, gotBotID string) (int, error) {
		require.Equal(t, "cli_message_denied_group", groupNo)
		require.Equal(t, botID, gotBotID)
		return 0, nil
	}
	var sendDispatched, typingDispatched bool
	ba.dispatchOverride = func(*config.MsgSendReq) (*config.MsgSendResp, error) {
		sendDispatched = true
		return &config.MsgSendResp{}, nil
	}
	ba.typingCMDDispatch = func(config.MsgCMDReq) error {
		typingDispatched = true
		return nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	actions := []string{"im.messages:write", "im.messages:read"}
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", "space_cli_message_bound", botID, actions)

	requests := []struct {
		name, method, path, body string
	}{
		{"send", http.MethodPost, "/v2/cli/messages", `{"channel_id":"cli_message_denied_group","channel_type":2,"payload":{"type":1,"content":"hello"}}`},
		{"edit", http.MethodPatch, "/v2/cli/messages/123", `{"message_id":"123","message_seq":1,"channel_id":"cli_message_denied_group","channel_type":2,"content_edit":"edited"}`},
		{"typing", http.MethodPost, "/v2/cli/typing", `{"channel_id":"cli_message_denied_group","channel_type":2}`},
		{"read_receipt", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"cli_message_denied_group","channel_type":2,"message_ids":["11"]}`},
		{"sync", http.MethodPost, "/v2/cli/messages/_sync", `{"channel_id":"cli_message_denied_group","channel_type":2,"limit":50}`},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			response := doCLIBotMessageRequest(t, target, test.method, test.path, test.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
	require.False(t, sendDispatched, "a Bot outside the target group must not dispatch messages")
	require.False(t, typingDispatched, "a Bot outside the target group must not dispatch typing")
}

func TestCLIBotTypingUsesBoundBotAndRejectsOBO(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID   = "cli_message_typing_bot"
		spaceID = "space_cli_message_typing"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)

	ba := NewBotAPI(ctx)
	var dispatched []config.MsgCMDReq
	ba.typingCMDDispatch = func(req config.MsgCMDReq) error {
		dispatched = append(dispatched, req)
		return nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:write"})

	response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/typing",
		`{"channel_id":"`+ownerID+`","channel_type":1}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Len(t, dispatched, 1)
	require.Equal(t, botID, dispatched[0].Param["from_uid"])
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Contains(t, envelope, "data")

	oboResponse := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/typing",
		`{"channel_id":"`+ownerID+`","channel_type":1,"on_behalf_of":"another-user"}`)
	require.Equal(t, http.StatusBadRequest, oboResponse.Code, oboResponse.Body.String())
	require.Len(t, dispatched, 1, "v2 must never dispatch OBO typing")
}

type cliBotMessageIMStub struct {
	server *httptest.Server
	calls  atomic.Int32
}

func newCLIBotMessageSyncStub(t *testing.T, resp *config.SyncChannelMessageResp) *cliBotMessageIMStub {
	t.Helper()
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	stub := &cliBotMessageIMStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channel/messagesync" {
			http.NotFound(w, r)
			return
		}
		stub.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func seedCLIBotGroup(t *testing.T, ctx *config.Context, groupNo, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, ?, ?)",
		groupNo, groupNo, status, spaceID,
	).Exec()
	require.NoError(t, err)
}

func seedCLIBotGroupMember(t *testing.T, ctx *config.Context, groupNo, botID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, is_deleted, version) VALUES (?, ?, 0, ?, 0, 1)",
		groupNo, botID, status,
	).Exec()
	require.NoError(t, err)
}

func cliBotSyncMessage(t *testing.T, seq int64, content, spaceID string) *config.MessageResp {
	t.Helper()
	payload := map[string]any{"type": 1, "content": content}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return &config.MessageResp{
		MessageID:   seq,
		MessageSeq:  uint32(seq),
		FromUID:     "cli_message_sync_peer",
		ChannelID:   "cli_message_sync_peer",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     raw,
	}
}

// TestCLIBotMessageWrongTypedFieldCannotBypassTokenSpaceCheck pins the same
// resource-guard fail-open branch as the user adapter: an unrelated field with
// the wrong JSON type must never skip the bound-Space check and reach the
// legacy Bot send handler. message_id is ignored by BotSendMessageReq, so only
// a fail-closed guard stops the cross-Space dispatch.
func TestCLIBotMessageWrongTypedFieldCannotBypassTokenSpaceCheck(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID      = "cli_message_wrong_type_bot"
		spaceID    = "space_cli_message_wrong_type_bound"
		otherSpace = "space_cli_message_wrong_type_other"
		groupNo    = "cli_message_wrong_type_group"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	seedCLIBotGroup(t, ctx, groupNo, otherSpace, group.GroupStatusNormal)
	seedCLIBotGroupMember(t, ctx, groupNo, botID, int(common.GroupMemberStatusNormal))

	ba := NewBotAPI(ctx)
	var sent []*config.MsgSendReq
	ba.dispatchOverride = func(req *config.MsgSendReq) (*config.MsgSendResp, error) {
		sent = append(sent, req)
		return &config.MsgSendResp{MessageID: 1, MessageSeq: 1, ClientMsgNo: "wrong-type"}, nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:write"})

	response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+groupNo+`","channel_type":2,"message_id":123,"payload":{"type":1,"content":"cross-space"}}`)

	require.Truef(t,
		response.Code == http.StatusBadRequest || response.Code == http.StatusForbidden,
		"a wrong-typed field must fail closed instead of skipping the Space gate, got %d: %s",
		response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error"`)
	require.Empty(t, sent, "the legacy Bot send handler must not run after a rejected guard request")
}

// TestCLIBotMessageSyncDoesNotLeakOtherSpaceDirectMessages covers the Bot
// messages/_sync DM path: WuKongIM stores DMs under a bare uid channel with no
// Space, so the adapter must apply the same payload.space_id filter the user
// channel-sync path uses. A Bot bound to one Space must not read a DM history
// tagged for another Space.
func TestCLIBotMessageSyncDoesNotLeakOtherSpaceDirectMessages(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID      = "cli_message_sync_space_bot"
		spaceID    = "space_cli_message_sync_bound"
		otherSpace = "space_cli_message_sync_other"
		peerID     = "cli_message_sync_peer"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	addSpaceMember(t, ctx, spaceID, peerID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
		botID, peerID,
	).Exec()
	require.NoError(t, err)

	stub := newCLIBotMessageSyncStub(t, &config.SyncChannelMessageResp{Messages: []*config.MessageResp{
		cliBotSyncMessage(t, 1, "bound-space-message", spaceID),
		cliBotSyncMessage(t, 2, "other-space-message", otherSpace),
	}})
	ctx.GetConfig().WuKongIM.APIURL = stub.server.URL

	ba := NewBotAPI(ctx)
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:read"})

	response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync",
		`{"channel_id":"`+peerID+`","channel_type":1,"limit":50}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 1, stub.calls.Load())

	var envelope struct {
		Data struct {
			Messages []struct {
				Payload []byte `json:"payload"`
			} `json:"messages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	contents := make([]string, 0, len(envelope.Data.Messages))
	for _, message := range envelope.Data.Messages {
		var payload struct {
			Content string `json:"content"`
		}
		require.NoError(t, json.Unmarshal(message.Payload, &payload))
		contents = append(contents, payload.Content)
	}
	require.Contains(t, contents, "bound-space-message")
	require.NotContains(t, contents, "other-space-message", "a DM history tagged for another Space must not leak through Bot sync")
}

// TestCLIBotMessageRejectsCaseVariantOBO covers the raw-key OBO check: Go's
// JSON decoder matches struct tags case-insensitively, so `On_Behalf_Of` would
// populate BotSendMessageReq.OnBehalfOf and delegate if the guard only looked up
// the exact lowercase key. A valid grant is pre-seeded to prove the request
// really would dispatch as the grantor, not merely fail the OBO authorization.
func TestCLIBotMessageRejectsCaseVariantOBO(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID   = "cli_message_obo_case_bot"
		spaceID = "space_cli_message_obo_case"
		groupNo = "cli_message_obo_case_group"
		grantor = "cli_message_obo_case_grantor"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	addSpaceMember(t, ctx, spaceID, grantor)
	seedCLIBotGroup(t, ctx, groupNo, spaceID, group.GroupStatusNormal)
	seedCLIBotGroupMember(t, ctx, groupNo, botID, int(common.GroupMemberStatusNormal))

	grants := newFakeOBOStore()
	grantID, err := grants.insertGrant(grantor, botID, "auto", "")
	require.NoError(t, err)
	enabled := 1
	require.NoError(t, grants.updateGrant(grantID, "", &enabled, nil))
	_, err = grants.insertScope(grantID, groupNo, common.ChannelTypeGroup.Uint8(), 1)
	require.NoError(t, err)

	ba := NewBotAPI(ctx)
	ba.oboStoreOverride = grants
	ba.oboChannelAccessOverride = func(string, string, uint8) (bool, error) { return true, nil }
	var sent []*config.MsgSendReq
	ba.dispatchOverride = func(req *config.MsgSendReq) (*config.MsgSendResp, error) {
		sent = append(sent, req)
		return &config.MsgSendResp{MessageID: 1, MessageSeq: 1, ClientMsgNo: "obo-case"}, nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:write"})

	response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+groupNo+`","channel_type":2,"On_Behalf_Of":"`+grantor+`","payload":{"type":1,"content":"delegated"}}`)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "On-behalf-of requests are not supported.")
	require.Empty(t, sent, "a case variant of on_behalf_of must not trigger v2 OBO delegation")
}

// TestCLIBotMessageSyncRejectsBlockedBot covers the active-membership
// predicate: a blacklisted Bot keeps its group_member row with is_deleted=0,
// so a count that only filters is_deleted would let it pull group and thread
// history. Both sub-surfaces must fail closed before any IM sync.
func TestCLIBotMessageSyncRejectsBlockedBot(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID   = "cli_message_blocked_bot"
		spaceID = "space_cli_message_blocked"
		groupNo = "cli_message_blocked_group"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	seedCLIBotGroup(t, ctx, groupNo, spaceID, group.GroupStatusNormal)
	seedCLIBotGroupMember(t, ctx, groupNo, botID, int(common.GroupMemberStatusBlacklist))

	stub := newCLIBotMessageSyncStub(t, &config.SyncChannelMessageResp{})
	ctx.GetConfig().WuKongIM.APIURL = stub.server.URL

	ba := NewBotAPI(ctx)
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:read"})

	requests := []struct {
		name, body string
	}{
		{"group", `{"channel_id":"` + groupNo + `","channel_type":2,"limit":50}`},
		{"thread", `{"channel_id":"` + groupNo + `____123456789012345","channel_type":5,"limit":50}`},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync", request.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.Zero(t, stub.calls.Load(), "a blacklisted Bot must not reach IM history sync")
		})
	}
}

// TestCLIBotMessageSyncKeepsDisbandedGroupHistoryReadable pins the old
// read-after-disband semantics: groups are read-only, not inaccessible, and
// the existing Bot sync handler has no disband gate. The v2 guard must not
// turn a disbanded group into a 404 for the read action.
func TestCLIBotMessageSyncKeepsDisbandedGroupHistoryReadable(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID   = "cli_message_disband_read_bot"
		spaceID = "space_cli_message_disband_read"
		groupNo = "cli_message_disband_read_group"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	seedCLIBotGroup(t, ctx, groupNo, spaceID, group.GroupStatusDisband)
	seedCLIBotGroupMember(t, ctx, groupNo, botID, int(common.GroupMemberStatusNormal))

	stub := newCLIBotMessageSyncStub(t, &config.SyncChannelMessageResp{})
	ctx.GetConfig().WuKongIM.APIURL = stub.server.URL

	ba := NewBotAPI(ctx)
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:read"})

	requests := []struct {
		name, body string
	}{
		{"group", `{"channel_id":"` + groupNo + `","channel_type":2,"limit":50}`},
		{"thread", `{"channel_id":"` + groupNo + `____123456789012345","channel_type":5,"limit":50}`},
	}
	for index, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync", request.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.EqualValues(t, index+1, stub.calls.Load())
		})
	}
}

// TestCLIBotBareUIDDMCannotBypassFriendOrTokenSpace pins both legacy and v2
// DM gates for the bound Bot. Space membership must not satisfy the Bot's own
// friend gate: a same-Space peer without a friend row is rejected by the
// legacy Bot send permission (wire 400 parity) and never dispatched. A friend
// that only belongs to another Space is rejected earlier by the v2 Token Space
// guard, so friendship cannot widen the Bot beyond its bound Space either.
func TestCLIBotBareUIDDMCannotBypassFriendOrTokenSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID            = "cli_message_dm_gate_bot"
		spaceID          = "space_cli_message_dm_gate"
		otherSpace       = "space_cli_message_dm_gate_other"
		sameSpacePeerID  = "cli_message_dm_same_space_peer"
		otherSpacePeerID = "cli_message_dm_other_space_peer"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	addSpaceMember(t, ctx, spaceID, sameSpacePeerID)
	addSpaceMember(t, ctx, otherSpace, otherSpacePeerID)
	for _, pair := range [][2]string{{botID, otherSpacePeerID}, {otherSpacePeerID, botID}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}

	ba := NewBotAPI(ctx)
	var sent []*config.MsgSendReq
	ba.dispatchOverride = func(req *config.MsgSendReq) (*config.MsgSendResp, error) {
		sent = append(sent, req)
		return &config.MsgSendResp{MessageID: 1, MessageSeq: 1, ClientMsgNo: "dm-gate"}, nil
	}
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID, []string{"im.messages:write"})

	requests := []struct {
		name, peer string
		status     int
	}{
		{"same_space_non_friend", sameSpacePeerID, http.StatusForbidden},
		{"other_space_friend", otherSpacePeerID, http.StatusForbidden},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
				`{"channel_id":"`+request.peer+`","channel_type":1,"payload":{"type":1,"content":"dm"}}`)
			require.Equal(t, request.status, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"error"`)
			require.Empty(t, sent, "a denied bare-uid DM must never dispatch")
		})
	}
}

// TestCLIBotEditAndReadReceiptDoNotAffectOtherSpaceMessages pins the Bot
// write/read boundary. The bound Bot is an active member of the other-Space
// group and friends with the other-Space DM peer, so only the Token Space can
// deny the request. Edit and read-receipt must fail before the legacy handlers
// write message_extra or member_readed.
func TestCLIBotEditAndReadReceiptDoNotAffectOtherSpaceMessages(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID      = "cli_message_xspace_edit_bot"
		spaceID    = "space_cli_message_xspace_bound"
		otherSpace = "space_cli_message_xspace_other"
		groupNo    = "cli_message_xspace_edit_group"
		messageID  = "9101"
		peerID     = "cli_message_xspace_edit_peer"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	addSpaceMember(t, ctx, otherSpace, peerID)
	seedCLIBotGroup(t, ctx, groupNo, otherSpace, group.GroupStatusNormal)
	seedCLIBotGroupMember(t, ctx, groupNo, botID, int(common.GroupMemberStatusNormal))
	for _, pair := range [][2]string{{botID, peerID}, {peerID, botID}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		messageID, 1, groupNo, int(common.ChannelTypeGroup.Uint8()), "original-edit", "original-hash", 1, 1,
	).Exec()
	require.NoError(t, err)

	stub := newCLIBotMessageSyncStub(t, &config.SyncChannelMessageResp{})
	ctx.GetConfig().WuKongIM.APIURL = stub.server.URL

	ba := NewBotAPI(ctx)
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID,
		[]string{"im.messages:write", "im.messages:read"})

	requests := []struct {
		name, method, path, body string
	}{
		{"edit_group", http.MethodPatch, "/v2/cli/messages/" + messageID, `{"message_id":"` + messageID + `","message_seq":1,"channel_id":"` + groupNo + `","channel_type":2,"content_edit":"cross-space-edit"}`},
		{"read_receipt_group", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"` + groupNo + `","channel_type":2,"message_ids":["` + messageID + `"]}`},
		{"edit_dm", http.MethodPatch, "/v2/cli/messages/9102", `{"message_id":"9102","message_seq":1,"channel_id":"` + peerID + `","channel_type":1,"content_edit":"cross-space-edit"}`},
		{"read_receipt_dm", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"` + peerID + `","channel_type":1,"message_ids":["9102"]}`},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIBotMessageRequest(t, target, request.method, request.path, request.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"FORBIDDEN"`)
		})
	}

	var contentEdit string
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", messageID).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "original-edit", contentEdit, "a Bot edit from another Space must not rewrite the stored content")

	var extraCount int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM message_extra WHERE message_id=?", "9102").LoadOne(&extraCount)
	require.NoError(t, err)
	require.Zero(t, extraCount, "a Bot edit targeting another Space must not create edit state")

	var readCount int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id IN (?, ?)",
		botID, messageID, "9102",
	).LoadOne(&readCount)
	require.NoError(t, err)
	require.Zero(t, readCount, "a Bot read receipt targeting another Space must not create read state")

	require.Zero(t, stub.calls.Load(), "rejected Bot edit/read requests must not reach IM")
}

// cliBotMessageSearchStub answers the IM query endpoint the Bot edit and
// read-receipt handlers call. The Space/membership decisions still run against
// the real DB fixture; the stub only supplies the message row a handler would
// otherwise fetch from WuKongIM.
type cliBotMessageSearchStub struct {
	server *httptest.Server
}

func newCLIBotMessageSearchStub(t *testing.T, messages []*config.MessageResp) *cliBotMessageSearchStub {
	t.Helper()
	response, err := json.Marshal(&config.SyncChannelMessageResp{Messages: messages})
	require.NoError(t, err)
	stub := &cliBotMessageSearchStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			_, _ = w.Write(response)
		case "/message/send":
			_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"cli-bot-message-test"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func cliBotPersonMessage(t *testing.T, messageID int64, messageSeq uint32, fromUID, spaceID string) *config.MessageResp {
	t.Helper()
	payload := map[string]any{"type": 1, "content": "dm message"}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return &config.MessageResp{
		MessageID:   messageID,
		MessageSeq:  messageSeq,
		FromUID:     fromUID,
		ChannelID:   "cli_message_bot_dm_payload_peer",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     raw,
	}
}

// TestCLIBotDirectMessageEditAndReadReceiptRespectPayloadSpace pins the same
// DM message-level Space rule for the bound Bot. The peer is an active member
// of the bound Space and friends with the Bot, so the channel guard allows the
// request; edit and read-receipt must still not act on a message whose payload
// is tagged for another Space.
func TestCLIBotDirectMessageEditAndReadReceiptRespectPayloadSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID      = "cli_message_dm_payload_bot"
		spaceID    = "space_cli_message_dm_payload_bound"
		otherSpace = "space_cli_message_dm_payload_other"
		peerID     = "cli_message_bot_dm_payload_peer"
	)
	insertUpstreamBot(t, ctx, botID)
	ownerID := "owner_" + botID
	addSpaceMember(t, ctx, spaceID, ownerID)
	addSpaceMember(t, ctx, spaceID, peerID)
	for _, pair := range [][2]string{{botID, peerID}, {peerID, botID}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}

	ba := NewBotAPI(ctx)
	source := newCLIBotMessageSource(t, ctx, ba)
	target := newCLIBotMessageTarget(t, source, ownerID, "Owner", spaceID, botID,
		[]string{"im.messages:write", "im.messages:read"})

	t.Run("edit", func(t *testing.T) {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			"9501", 51, peerID, int(common.ChannelTypePerson.Uint8()), "original-payload-edit", "original-payload-hash", 1, 1,
		).Exec()
		require.NoError(t, err)
		stub := newCLIBotMessageSearchStub(t, []*config.MessageResp{
			cliBotPersonMessage(t, 9501, 51, botID, otherSpace),
		})
		ctx.GetConfig().WuKongIM.APIURL = stub.server.URL
		response := doCLIBotMessageRequest(t, target, http.MethodPatch, "/v2/cli/messages/9501",
			`{"message_id":"9501","message_seq":51,"channel_id":"`+peerID+`","channel_type":1,"content_edit":"cross-space-edit"}`)
		require.NotEqual(t, http.StatusOK, response.Code,
			"editing a DM message tagged for another Space must not report success: %s", response.Body.String())
		var contentEdit string
		err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", "9501").LoadOne(&contentEdit)
		require.NoError(t, err)
		require.Equal(t, "original-payload-edit", contentEdit,
			"a Bot edit of a DM message tagged for another Space must not rewrite its stored content")
	})

	t.Run("read_receipt", func(t *testing.T) {
		stub := newCLIBotMessageSearchStub(t, []*config.MessageResp{
			cliBotPersonMessage(t, 9502, 52, peerID, otherSpace),
		})
		ctx.GetConfig().WuKongIM.APIURL = stub.server.URL
		doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_read_receipt",
			`{"channel_id":"`+peerID+`","channel_type":1,"message_ids":["9502"]}`)
		var readCount int
		err := ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id=?",
			botID, "9502",
		).LoadOne(&readCount)
		require.NoError(t, err)
		require.Zero(t, readCount,
			"a Bot read receipt on a DM message tagged for another Space must not create read state")
	})
}

// newCLIBotMessageSeqIMStub resolves /messages by message_seqs, allowing the
// duplicate-case PATCH regression to distinguish the guard's query from the
// legacy edit handler's query.
func newCLIBotMessageSeqIMStub(t *testing.T, messagesBySeq map[uint32]*config.MessageResp) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			var request struct {
				MessageSeqs []uint32 `json:"message_seqs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid message query", http.StatusBadRequest)
				return
			}
			messages := make([]*config.MessageResp, 0, len(request.MessageSeqs))
			for _, messageSeq := range request.MessageSeqs {
				if message := messagesBySeq[messageSeq]; message != nil {
					messages = append(messages, message)
				}
			}
			_ = json.NewEncoder(w).Encode(&config.SyncChannelMessageResp{Messages: messages})
		case "/message/send":
			_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"cli-bot-message-test"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestCLIBotEditDuplicateCaseSequenceCannotCrossSpace proves that a missing
// body message_id cannot make the Bot guard and legacy handler resolve
// different message_seq values through case-variant JSON keys.
func TestCLIBotEditDuplicateCaseSequenceCannotCrossSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID      = "cli_message_dup_seq_bot"
		boundSpace = "space_cli_bot_dup_seq_bound"
		otherSpace = "space_cli_bot_dup_seq_other"
		peerID     = "cli_message_bot_dm_payload_peer"
		ownerID    = "owner_cli_message_dup_seq_bot"
		boundID    = int64(9701)
		otherID    = "9702"
		boundSeq   = uint32(71)
		otherSeq   = uint32(72)
	)
	insertUpstreamBot(t, ctx, botID)
	addSpaceMember(t, ctx, boundSpace, ownerID)
	addSpaceMember(t, ctx, boundSpace, peerID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		otherID, otherSeq, peerID, int(common.ChannelTypePerson.Uint8()), "original-other-space-edit", "original-other-space-hash", 1, 1,
	).Exec()
	require.NoError(t, err)

	im := newCLIBotMessageSeqIMStub(t, map[uint32]*config.MessageResp{
		boundSeq: cliBotPersonMessage(t, boundID, boundSeq, botID, boundSpace),
		otherSeq: cliBotPersonMessage(t, 9702, otherSeq, botID, otherSpace),
	})
	ctx.GetConfig().WuKongIM.APIURL = im.URL
	ba := NewBotAPI(ctx)
	target := newCLIBotMessageTarget(t, newCLIBotMessageSource(t, ctx, ba), ownerID, "Owner", boundSpace, botID,
		[]string{"im.messages:write"})

	// The guard sees boundSeq in the original body. Re-encoding the body to add
	// the path message_id sorts the case variants, leaving otherSeq for legacy.
	response := doCLIBotMessageRequest(t, target, http.MethodPatch, "/v2/cli/messages/"+otherID,
		`{"channel_id":"`+peerID+`","channel_type":1,"message_seq":72,"MESSAGE_SEQ":71,"content_edit":"cross-space-edit"}`)

	require.NotEqual(t, http.StatusOK, response.Code,
		"case-variant sequence keys must not make the Bot handler edit a different-Space message: %s", response.Body.String())
	var contentEdit string
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", otherID).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "original-other-space-edit", contentEdit,
		"a cross-Space message must remain unchanged when duplicate-case sequence keys are supplied")
}

// TestCLIBotUnlabeledDirectMessageUsesOwnerDefaultSpace pins the legacy
// unlabeled-DM visibility boundary for a User Bot: the owner's default Space A
// may see the history, but a token bound to Space B must not see or mutate it.
func TestCLIBotUnlabeledDirectMessageUsesOwnerDefaultSpace(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		botID     = "cli_message_default_space_bot"
		ownerID   = "owner_cli_message_default_space_bot"
		peerID    = "cli_message_bot_dm_payload_peer"
		defaultA  = "space_cli_bot_default_a"
		boundB    = "space_cli_bot_default_b"
		syncID    = int64(9800)
		editID    = int64(9801)
		editIDStr = "9801"
		editSeq   = uint32(81)
		readID    = int64(9802)
		readIDStr = "9802"
	)
	insertUpstreamBot(t, ctx, botID)
	addSpaceMember(t, ctx, defaultA, ownerID)
	addSpaceMember(t, ctx, boundB, ownerID)
	addSpaceMember(t, ctx, defaultA, peerID)
	addSpaceMember(t, ctx, boundB, peerID)
	// The default Space is determined by the owner's earliest active
	// space_member.created_at, not by the Bot's membership (Bots have none).
	_, err := ctx.DB().UpdateBySql(
		"UPDATE space_member SET created_at=? WHERE space_id=? AND uid=?",
		"2020-01-01 00:00:00", defaultA, ownerID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE space_member SET created_at=? WHERE space_id=? AND uid=?",
		"2020-01-02 00:00:00", boundB, ownerID,
	).Exec()
	require.NoError(t, err)
	for _, pair := range [][2]string{{botID, peerID}, {peerID, botID}} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		editIDStr, editSeq, peerID, int(common.ChannelTypePerson.Uint8()), "original-unlabeled-edit", "original-unlabeled-hash", 1, 1,
	).Exec()
	require.NoError(t, err)

	ba := NewBotAPI(ctx)
	targetA := newCLIBotMessageTarget(t, newCLIBotMessageSource(t, ctx, ba), ownerID, "Owner", defaultA, botID,
		[]string{"im.messages:write", "im.messages:read"})
	targetB := newCLIBotMessageTarget(t, newCLIBotMessageSource(t, ctx, ba), ownerID, "Owner", boundB, botID,
		[]string{"im.messages:write", "im.messages:read"})

	syncStub := newCLIBotMessageSyncStub(t, &config.SyncChannelMessageResp{Messages: []*config.MessageResp{
		cliBotSyncMessage(t, syncID, "legacy-unlabeled-history", ""),
	}})
	ctx.GetConfig().WuKongIM.APIURL = syncStub.server.URL
	readSyncContents := func(target http.Handler) []string {
		response := doCLIBotMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync",
			`{"channel_id":"`+peerID+`","channel_type":1,"limit":50}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var envelope struct {
			Data struct {
				Messages []*config.MessageResp `json:"messages"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		contents := make([]string, 0, len(envelope.Data.Messages))
		for _, message := range envelope.Data.Messages {
			var payload struct {
				Content string `json:"content"`
			}
			require.NoError(t, json.Unmarshal(message.Payload, &payload))
			contents = append(contents, payload.Content)
		}
		return contents
	}
	require.NotContains(t, readSyncContents(targetB), "legacy-unlabeled-history",
		"a token bound to B must not see unlabeled history owned by the owner's default Space A")
	require.Contains(t, readSyncContents(targetA), "legacy-unlabeled-history",
		"the owner's default Space A must retain visibility of legacy unlabeled DM history")

	editStub := newCLIBotMessageSearchStub(t, []*config.MessageResp{
		cliBotPersonMessage(t, editID, editSeq, botID, ""),
	})
	ctx.GetConfig().WuKongIM.APIURL = editStub.server.URL
	editResponse := doCLIBotMessageRequest(t, targetB, http.MethodPatch, "/v2/cli/messages/"+editIDStr,
		`{"message_id":"`+editIDStr+`","message_seq":81,"channel_id":"`+peerID+`","channel_type":1,"content_edit":"cross-space-edit"}`)
	require.Equal(t, http.StatusForbidden, editResponse.Code, editResponse.Body.String())
	var contentEdit string
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", editIDStr).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "original-unlabeled-edit", contentEdit,
		"a token bound to B must not edit an unlabeled message visible only in default Space A")
	editResponse = doCLIBotMessageRequest(t, targetA, http.MethodPatch, "/v2/cli/messages/"+editIDStr,
		`{"message_id":"`+editIDStr+`","message_seq":81,"channel_id":"`+peerID+`","channel_type":1,"content_edit":"default-space-edit"}`)
	require.Equal(t, http.StatusOK, editResponse.Code, editResponse.Body.String())
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", editIDStr).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "default-space-edit", contentEdit,
		"the owner's default Space must retain access to its unlabeled DM")

	readStub := newCLIBotMessageSearchStub(t, []*config.MessageResp{
		cliBotPersonMessage(t, readID, 82, peerID, ""),
	})
	ctx.GetConfig().WuKongIM.APIURL = readStub.server.URL
	readResponse := doCLIBotMessageRequest(t, targetB, http.MethodPost, "/v2/cli/messages/_read_receipt",
		`{"channel_id":"`+peerID+`","channel_type":1,"message_ids":["`+readIDStr+`"]}`)
	require.Equal(t, http.StatusForbidden, readResponse.Code, readResponse.Body.String())
	var readCount int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id=?",
		botID, readIDStr,
	).LoadOne(&readCount)
	require.NoError(t, err)
	require.Zero(t, readCount,
		"a token bound to B must not mark an unlabeled message from default Space A as read")
	readResponse = doCLIBotMessageRequest(t, targetA, http.MethodPost, "/v2/cli/messages/_read_receipt",
		`{"channel_id":"`+peerID+`","channel_type":1,"message_ids":["`+readIDStr+`"]}`)
	require.Equal(t, http.StatusOK, readResponse.Code, readResponse.Body.String())
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id=?",
		botID, readIDStr,
	).LoadOne(&readCount)
	require.NoError(t, err)
	require.Equal(t, 1, readCount,
		"the owner's default Space must retain read-receipt access to its unlabeled DM")
}
