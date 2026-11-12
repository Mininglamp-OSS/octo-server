package message_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/bot_api"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

const (
	cliMessageTestToken         = "cli-message-business-token"
	cliMessageTestServiceSecret = "cli-message-service-secret-with-at-least-32-bytes"
)

func seedCLIMessageSpace(t *testing.T, ctx *config.Context, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, 1, NOW(), NOW())",
		spaceID, spaceID, testutil.UID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, testutil.UID,
	).Exec()
	require.NoError(t, err)
}

func seedCLIMessageSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
}

func newCLIMessageTarget(t *testing.T, source *wkhttp.WKHttp, ownerID, ownerName, spaceID string, actions []string) *wkhttp.WKHttp {
	t.Helper()
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/internal/tokens/_resolve", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer "+cliMessageTestServiceSecret, r.Header.Get("Authorization"))
		var request struct {
			Token string `json:"token"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, cliMessageTestToken, request.Token)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"principal": map[string]any{
					"token_id": "cli-message-token-id", "owner_id": ownerID,
					"owner_name": ownerName, "space_id": spaceID,
					"subject_type": "user", "subject_id": ownerID,
					"issued_at": "2026-01-01T00:00:00Z",
				},
				"permissions":  []map[string]any{{"domain": "im.messages", "actions": actions}},
				"bot_bindings": []map[string]string{},
			},
		}))
	}))
	t.Cleanup(authServer.Close)
	target, err := cliv2.NewRouter(authServer.URL, cliMessageTestServiceSecret)
	require.NoError(t, err)
	require.NoError(t, cliv2.SetUIDRateLimiter(target, func(c *wkhttp.Context) {
		principal := cliv2.PrincipalFrom(c)
		require.Equal(t, ownerID, principal.UserID)
		require.Equal(t, spaceID, principal.SpaceID)
		require.Empty(t, principal.BotID)
		require.Equal(t, ownerID, c.GetLoginUID())
		c.Next()
	}))
	require.NoError(t, cliv2.Mount(source, target))
	return target
}

func doCLIMessageRequest(t *testing.T, target http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+cliMessageTestToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	target.ServeHTTP(response, request)
	return response
}

func TestCLISendUserRejectsInteractiveCard(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	m := message.New(ctx)
	t.Cleanup(m.Stop)
	source := wkhttp.New()
	m.Route(source)
	file.New(ctx).Route(source)
	bot_api.NewBotAPI(ctx).Route(source)
	target := newCLIMessageTarget(t, source, testutil.UID, "User", "space_A", []string{"im.messages:write"})

	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"peer","channel_type":1,"payload":{"type":17,"card":{}},"from_uid":"forged-user"}`)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), errcode.ErrMessageCardSendForbidden.DefaultMessage)
}

func TestCLIV2MessageOperationsRejectGroupsOutsideTokenSpace(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	m := message.New(ctx)
	t.Cleanup(m.Stop)
	const (
		boundSpace = "space_cli_bound"
		otherSpace = "space_cli_other"
		groupNo    = "cli_message_other_space_group"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpace(t, ctx, otherSpace)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, 1, ?)",
		groupNo, groupNo, otherSpace,
	).Exec()
	require.NoError(t, err)

	source := wkhttp.New()
	m.Route(source)
	file.New(ctx).Route(source)
	bot_api.NewBotAPI(ctx).Route(source)
	target := newCLIMessageTarget(t, source, testutil.UID, "User", boundSpace, []string{
		"im.messages:write",
		"im.messages:read",
	})

	channelType := strconv.Itoa(int(common.ChannelTypeGroup.Uint8()))
	requests := []struct {
		name, method, path, body string
	}{
		{"send", http.MethodPost, "/v2/cli/messages", `{"channel_id":"` + groupNo + `","channel_type":` + channelType + `,"payload":{"type":1,"content":"hello"}}`},
		{"edit", http.MethodPatch, "/v2/cli/messages/123", `{"channel_id":"` + groupNo + `","channel_type":` + channelType + `,"message_seq":1,"content_edit":"edited"}`},
		{"sync", http.MethodPost, "/v2/cli/messages/_sync", `{"channel_id":"` + groupNo + `","channel_type":` + channelType + `,"limit":50}`},
		{"read_receipt", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"` + groupNo + `","channel_type":` + channelType + `,"message_ids":["123"]}`},
		{"typing", http.MethodPost, "/v2/cli/typing", `{"channel_id":"` + groupNo + `","channel_type":` + channelType + `}`},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			response := doCLIMessageRequest(t, target, test.method, test.path, test.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
}

// cliMessageIMStub replaces only the outbound WuKongIM transport. The message
// handlers and their Space/membership/disband queries still run against the
// real module instances and the migrated MySQL fixture, so a captured dispatch
// proves the server actually decided to send, not that a mocked guard echoed.
type cliMessageIMStub struct {
	server *httptest.Server
	sends  atomic.Int32
	syncs  atomic.Int32
}

func newCLIMessageIMStub(t *testing.T) *cliMessageIMStub {
	t.Helper()
	stub := &cliMessageIMStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/message/send":
			stub.sends.Add(1)
			_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"cli-message-test"}}`))
		case "/channel/messagesync":
			stub.syncs.Add(1)
			_, _ = w.Write([]byte(`{"start_message_seq":0,"end_message_seq":0,"pull_mode":0,"messages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func newCLIMessageSource(t *testing.T, ctx *config.Context) *wkhttp.WKHttp {
	t.Helper()
	m := message.New(ctx)
	t.Cleanup(m.Stop)
	source := wkhttp.New()
	m.Route(source)
	file.New(ctx).Route(source)
	bot_api.NewBotAPI(ctx).Route(source)
	return source
}

func seedCLIMessageGroup(t *testing.T, ctx *config.Context, groupNo, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, ?, ?)",
		groupNo, groupNo, status, spaceID,
	).Exec()
	require.NoError(t, err)
}

func seedCLIMessageGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, status int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, is_deleted, version) VALUES (?, ?, 0, ?, 0, 1)",
		groupNo, uid, status,
	).Exec()
	require.NoError(t, err)
}

// seedCLIMessageOwnBot creates the active User Bot row and its user mirror,
// including the two friend rows production bot creation writes. The bot
// deliberately has no space_member row: the DM gate must recognise it as the
// caller's own Bot (or honour the established friend relation) instead of
// demanding that the Bot itself be a Space member.
func seedCLIMessageOwnBot(t *testing.T, ctx *config.Context, ownerID, botID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user (uid, name, username, status, robot, short_no, zone, phone) VALUES (?, ?, ?, 1, 1, ?, '', '')",
		botID, botID, botID, util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, username, token, version, status, creator_uid, bot_token, im_token_cache, bot_commands) VALUES (?, ?, 'test_token', 1, 1, ?, ?, '', '[]')",
		botID, botID, ownerID, "bf_"+botID,
	).Exec()
	require.NoError(t, err)
	for _, pair := range [][2]string{{ownerID, botID}, {botID, ownerID}} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}
}

// TestCLIV2WrongTypedFieldCannotBypassTokenSpaceCheck covers the guard's
// fail-open JSON error branch: a wrong-typed extra field previously made the
// resource guard return to the legacy handler before the cross-Space check.
// The send handler ignores message_id, so the only consumer-visible difference
// must be a fail-closed error and no IM dispatch.
func TestCLIV2WrongTypedFieldCannotBypassTokenSpaceCheck(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	const (
		boundSpace = "space_cli_wrong_type_bound"
		otherSpace = "space_cli_wrong_type_other"
		groupNo    = "cli_message_wrong_type_group"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpace(t, ctx, otherSpace)
	seedCLIMessageGroup(t, ctx, groupNo, otherSpace, group.GroupStatusNormal)
	seedCLIMessageGroupMember(t, ctx, groupNo, testutil.UID, int(common.GroupMemberStatusNormal))

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+groupNo+`","channel_type":2,"message_id":123,"payload":{"type":1,"content":"cross-space"}}`)

	require.Truef(t,
		response.Code == http.StatusBadRequest || response.Code == http.StatusForbidden,
		"a wrong-typed field must fail closed instead of skipping the Space gate, got %d: %s",
		response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error"`)
	require.Zero(t, im.sends.Load(), "the legacy send handler must not run after a rejected guard request")
}

// TestCLIV2SyncWrongTypedFieldCannotBypassTokenSpaceCheck is the reviewer's
// exact repro shape: the read handler ignores `message_id`, so a wrong-typed
// message_id must still be rejected by the guard rather than skipping the
// bound-Space check and returning another Space's group history.
func TestCLIV2SyncWrongTypedFieldCannotBypassTokenSpaceCheck(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		boundSpace = "space_cli_sync_wrong_type_bound"
		otherSpace = "space_cli_sync_wrong_type_other"
		groupNo    = "cli_message_sync_wrong_type_group"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpace(t, ctx, otherSpace)
	seedCLIMessageGroup(t, ctx, groupNo, otherSpace, group.GroupStatusNormal)
	seedCLIMessageGroupMember(t, ctx, groupNo, testutil.UID, int(common.GroupMemberStatusNormal))

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:read"})

	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync",
		`{"channel_id":"`+groupNo+`","channel_type":2,"message_id":123,"limit":50}`)

	require.Truef(t,
		response.Code == http.StatusBadRequest || response.Code == http.StatusForbidden,
		"a wrong-typed field must fail closed instead of skipping the Space gate, got %d: %s",
		response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"error"`)
	require.Zero(t, im.syncs.Load(), "the legacy sync handler must not run after a rejected guard request")
}

// TestCLIV2DirectMessagesToSelfAndOwnBotRemainAvailable pins the legitimate
// DM exemptions that the v1 read gate already grants (personDMAccessDecision:
// notes-to-self and own Bot). The v2 guard must not regress them into
// CheckBothMembers-only denials.
func TestCLIV2DirectMessagesToSelfAndOwnBotRemainAvailable(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	const (
		boundSpace = "space_cli_dm_bound"
		ownBotID   = "cli_message_owned_bot"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageOwnBot(t, ctx, testutil.UID, ownBotID)

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	requests := []struct {
		name, body string
	}{
		{"self", `{"channel_id":"` + testutil.UID + `","channel_type":1,"payload":{"type":1,"content":"note to self"}}`},
		{"own_bot", `{"channel_id":"` + ownBotID + `","channel_type":1,"payload":{"type":1,"content":"hello bot"}}`},
	}
	for index, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages", request.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.EqualValues(t, index+1, im.sends.Load())
		})
	}
}

// TestCLIV2UserCannotSendToDisbandedGroupOrThread covers the disbanded-group
// read-only rule on the user send path. The v2 adapter calls the dispatch
// service directly, so the guard must carry the same status check the v1
// sendMsg handler performs.
func TestCLIV2UserCannotSendToDisbandedGroupOrThread(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	const (
		boundSpace = "space_cli_disband_bound"
		groupNo    = "cli_message_disbanded_group"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageGroup(t, ctx, groupNo, boundSpace, group.GroupStatusDisband)
	seedCLIMessageGroupMember(t, ctx, groupNo, testutil.UID, int(common.GroupMemberStatusNormal))

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	requests := []struct {
		name, body string
	}{
		{"group", `{"channel_id":"` + groupNo + `","channel_type":2,"payload":{"type":1,"content":"to disbanded group"}}`},
		{"thread", `{"channel_id":"` + groupNo + `____123456789012345","channel_type":5,"payload":{"type":1,"content":"to disbanded thread"}}`},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages", request.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"FORBIDDEN"`)
			require.Zero(t, im.sends.Load(), "a disbanded group/thread must not dispatch")
		})
	}
}

// TestCLIV2BareUIDSameSpaceNonFriendCannotBypassFriendGate pins the legacy
// personal-mode DM rule. A bare uid channel carries no Space prefix, so the v1
// /v1/message/send path requires the two-way friend relation for it. Sharing
// the Token-bound Space must not replace that gate: the caller and the peer are
// both active members of the bound Space but have no friend rows in either
// direction, so the send must be denied before the legacy dispatch runs.
func TestCLIV2BareUIDSameSpaceNonFriendCannotBypassFriendGate(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	const (
		boundSpace = "space_cli_dm_nonfriend_bound"
		peerID     = "cli_message_dm_nonfriend_peer"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpaceMember(t, ctx, boundSpace, peerID)

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+peerID+`","channel_type":1,"payload":{"type":1,"content":"non-friend dm"}}`)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"FORBIDDEN"`)
	require.Zero(t, im.sends.Load(),
		"a same-Space non-friend bare-uid DM must not dispatch without the legacy friend gate")
}

func TestCLIV2SyncSameSpaceNonFriendKeepsLegacyAccess(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		boundSpace = "space_cli_dm_sync_nonfriend"
		peerID     = "cli_dm_sync_nonfriend_peer"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpaceMember(t, ctx, boundSpace, peerID)
	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:read"})
	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_sync",
		`{"channel_id":"`+peerID+`","channel_type":1,"limit":50}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 1, im.syncs.Load())
}

// TestCLIV2DifferentSpaceFriendCannotCrossTokenBoundSpace pins the Token Space
// boundary for personal-mode DM. The caller and the peer are friends in both
// directions, but the peer belongs to a different Space only. Friendship is the
// v1 compatibility gate for bare-uid channels, not a cross-Space authorization,
// so the request must be denied before any dispatch.
func TestCLIV2DifferentSpaceFriendCannotCrossTokenBoundSpace(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	ctx.GetConfig().Message.SendMessageOn = true
	const (
		boundSpace = "space_cli_dm_other_friend_bound"
		otherSpace = "space_cli_dm_other_friend_space"
		peerID     = "cli_message_dm_other_friend_peer"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, 1, NOW(), NOW())",
		otherSpace, otherSpace, peerID,
	).Exec()
	require.NoError(t, err)
	seedCLIMessageSpaceMember(t, ctx, otherSpace, peerID)
	for _, pair := range [][2]string{{testutil.UID, peerID}, {peerID, testutil.UID}} {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	response := doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages",
		`{"channel_id":"`+peerID+`","channel_type":1,"payload":{"type":1,"content":"cross-space friend dm"}}`)

	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"FORBIDDEN"`)
	require.Zero(t, im.sends.Load(),
		"a friend outside the Token-bound Space must not dispatch a bare-uid DM")
}

// TestCLIV2EditAndReadReceiptDoNotAffectOtherSpaceMessages pins the write/read
// boundary for messages owned by a channel outside the Token-bound Space. The
// caller is an active member of the other-Space group, so only the Space gate
// can stop the request. A 403 with untouched edit/read state proves the v2
// guard runs before the legacy edit and read-receipt handlers instead of
// letting a message_id from another Space take effect.
func TestCLIV2EditAndReadReceiptDoNotAffectOtherSpaceMessages(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		boundSpace = "space_cli_edit_bound"
		otherSpace = "space_cli_edit_other"
		groupNo    = "cli_message_edit_other_space_group"
		messageID  = "9001"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpace(t, ctx, otherSpace)
	seedCLIMessageGroup(t, ctx, groupNo, otherSpace, group.GroupStatusNormal)
	seedCLIMessageGroupMember(t, ctx, groupNo, testutil.UID, int(common.GroupMemberStatusNormal))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		messageID, 1, groupNo, int(common.ChannelTypeGroup.Uint8()), "original-edit", "original-hash", 1, 1,
	).Exec()
	require.NoError(t, err)

	im := newCLIMessageIMStub(t)
	ctx.GetConfig().WuKongIM.APIURL = im.server.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write", "im.messages:read"})

	threadChannel := groupNo + "____123456789012345"
	requests := []struct {
		name, method, path, body string
	}{
		{"edit_group", http.MethodPatch, "/v2/cli/messages/" + messageID, `{"channel_id":"` + groupNo + `","channel_type":2,"message_seq":1,"content_edit":"cross-space-edit"}`},
		{"read_receipt_group", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"` + groupNo + `","channel_type":2,"message_ids":["` + messageID + `"]}`},
		{"edit_thread", http.MethodPatch, "/v2/cli/messages/9002", `{"channel_id":"` + threadChannel + `","channel_type":5,"message_seq":1,"content_edit":"cross-space-edit"}`},
		{"read_receipt_thread", http.MethodPost, "/v2/cli/messages/_read_receipt", `{"channel_id":"` + threadChannel + `","channel_type":5,"message_ids":["9002"]}`},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			response := doCLIMessageRequest(t, target, request.method, request.path, request.body)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"FORBIDDEN"`)
		})
	}

	var contentEdit string
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", messageID).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "original-edit", contentEdit, "an edit from another Space must not rewrite the stored content")

	var extraCount int
	err = ctx.DB().SelectBySql("SELECT COUNT(*) FROM message_extra WHERE message_id=?", "9002").LoadOne(&extraCount)
	require.NoError(t, err)
	require.Zero(t, extraCount, "an edit targeting another Space must not create edit state")

	var readCount int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id IN (?, ?)",
		testutil.UID, messageID, "9002",
	).LoadOne(&readCount)
	require.NoError(t, err)
	require.Zero(t, readCount, "a read receipt targeting another Space must not create read state")

	require.Zero(t, im.sends.Load(), "a rejected edit must not dispatch to IM")
	require.Zero(t, im.syncs.Load(), "a rejected read receipt must not sync IM history")
}

// cliMessageSearchStub answers the IM query endpoint the user edit and
// read-receipt handlers call. It never stands in for the Space or membership
// decisions: those still run against the real DB fixture; the stub only proves
// whether a request reached the legacy handler after the v2 guard.
type cliMessageSearchStub struct {
	server *httptest.Server
}

func newCLIMessageSearchStub(t *testing.T, messages []*config.MessageResp) *cliMessageSearchStub {
	t.Helper()
	response, err := json.Marshal(&config.SyncChannelMessageResp{Messages: messages})
	require.NoError(t, err)
	stub := &cliMessageSearchStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			_, _ = w.Write(response)
		case "/message/send":
			_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"cli-message-test"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func cliMessagePersonMessage(t *testing.T, messageID int64, messageSeq uint32, fromUID, spaceID string) *config.MessageResp {
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
		ChannelID:   "cli_message_dm_payload_peer",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     raw,
	}
}

// TestCLIV2DirectMessageEditAndReadReceiptRespectPayloadSpace pins the DM
// message-level Space rule for edit and read receipts. WuKongIM stores DIRECT
// channels under a bare uid with no Space, so payload.space_id is the only
// authoritative marker. The peer is an active member of the bound Space and
// both accounts are friends, so the channel guard legitimately allows the
// request; the legacy handlers must still refuse to edit or mark read a
// message whose payload is tagged for another Space.
func TestCLIV2DirectMessageEditAndReadReceiptRespectPayloadSpace(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		boundSpace = "space_cli_dm_payload_bound"
		otherSpace = "space_cli_dm_payload_other"
		peerID     = "cli_message_dm_payload_peer"
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpaceMember(t, ctx, boundSpace, peerID)
	for _, pair := range [][2]string{{testutil.UID, peerID}, {peerID, testutil.UID}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write", "im.messages:read"})

	t.Run("edit", func(t *testing.T) {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			"9401", 41, peerID, int(common.ChannelTypePerson.Uint8()), "original-space-edit", "original-space-hash", 1, 1,
		).Exec()
		require.NoError(t, err)
		stub := newCLIMessageSearchStub(t, []*config.MessageResp{
			cliMessagePersonMessage(t, 9401, 41, testutil.UID, otherSpace),
		})
		ctx.GetConfig().WuKongIM.APIURL = stub.server.URL
		response := doCLIMessageRequest(t, target, http.MethodPatch, "/v2/cli/messages/9401",
			`{"channel_id":"`+peerID+`","channel_type":1,"message_seq":41,"content_edit":"cross-space-edit"}`)
		require.NotEqual(t, http.StatusOK, response.Code,
			"editing a DM message tagged for another Space must not report success: %s", response.Body.String())
		var contentEdit string
		err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", "9401").LoadOne(&contentEdit)
		require.NoError(t, err)
		require.Equal(t, "original-space-edit", contentEdit,
			"editing a DM message tagged for another Space must not rewrite its stored content")
	})

	t.Run("read_receipt", func(t *testing.T) {
		stub := newCLIMessageSearchStub(t, []*config.MessageResp{
			cliMessagePersonMessage(t, 9402, 42, peerID, otherSpace),
		})
		ctx.GetConfig().WuKongIM.APIURL = stub.server.URL
		doCLIMessageRequest(t, target, http.MethodPost, "/v2/cli/messages/_read_receipt",
			`{"channel_id":"`+peerID+`","channel_type":1,"message_ids":["9402"]}`)
		var readCount int
		err := ctx.DB().SelectBySql(
			"SELECT COUNT(*) FROM member_readed WHERE uid=? AND message_id=?",
			testutil.UID, "9402",
		).LoadOne(&readCount)
		require.NoError(t, err)
		require.Zero(t, readCount,
			"a read receipt on a DM message tagged for another Space must not create read state")
	})
}

// newCLIV2MessageSeqIMStub resolves /messages by message_seqs, allowing the
// duplicate-case PATCH regression to distinguish the guard's request from the
// legacy handler's request.
func newCLIV2MessageSeqIMStub(t *testing.T, messagesBySeq map[uint32]*config.MessageResp) *httptest.Server {
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
			_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"cli-message-test"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestCLIV2EditDuplicateCaseSequenceCannotCrossSpace proves that a missing
// body message_id cannot make the guard and legacy handler resolve different
// message_seq values through case-variant JSON keys.
func TestCLIV2EditDuplicateCaseSequenceCannotCrossSpace(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	t.Cleanup(func() { _ = testutil.CleanAllTables(ctx) })
	const (
		boundSpace = "space_cli_dup_seq_bound"
		otherSpace = "space_cli_dup_seq_other"
		peerID     = "cli_message_dup_seq_peer"
		boundID    = int64(9601)
		otherID    = "9602"
		boundSeq   = uint32(61)
		otherSeq   = uint32(62)
	)
	seedCLIMessageSpace(t, ctx, boundSpace)
	seedCLIMessageSpaceMember(t, ctx, boundSpace, peerID)
	for _, pair := range [][2]string{{testutil.UID, peerID}, {peerID, testutil.UID}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?, ?, 0)",
			pair[0], pair[1],
		).Exec()
		require.NoError(t, err)
	}
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO message_extra (message_id, message_seq, channel_id, channel_type, content_edit, content_edit_hash, edited_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		otherID, otherSeq, peerID, int(common.ChannelTypePerson.Uint8()), "original-other-space-edit", "original-other-space-hash", 1, 1,
	).Exec()
	require.NoError(t, err)

	im := newCLIV2MessageSeqIMStub(t, map[uint32]*config.MessageResp{
		boundSeq: cliMessagePersonMessage(t, boundID, boundSeq, testutil.UID, boundSpace),
		otherSeq: cliMessagePersonMessage(t, 9602, otherSeq, testutil.UID, otherSpace),
	})
	ctx.GetConfig().WuKongIM.APIURL = im.URL
	target := newCLIMessageTarget(t, newCLIMessageSource(t, ctx), testutil.UID, "User", boundSpace,
		[]string{"im.messages:write"})

	// In the raw body MESSAGE_SEQ is last, so the guard sees boundSeq. Once it
	// inserts the path message_id by re-encoding the map, JSON key sorting puts
	// MESSAGE_SEQ before message_seq, so the legacy handler sees otherSeq.
	response := doCLIMessageRequest(t, target, http.MethodPatch, "/v2/cli/messages/"+otherID,
		`{"channel_id":"`+peerID+`","channel_type":1,"message_seq":62,"MESSAGE_SEQ":61,"content_edit":"cross-space-edit"}`)

	require.NotEqual(t, http.StatusOK, response.Code,
		"case-variant sequence keys must not make the legacy handler edit a different-Space message: %s", response.Body.String())
	var contentEdit string
	err = ctx.DB().SelectBySql("SELECT content_edit FROM message_extra WHERE message_id=?", otherID).LoadOne(&contentEdit)
	require.NoError(t, err)
	require.Equal(t, "original-other-space-edit", contentEdit,
		"a cross-Space message must remain unchanged when duplicate-case sequence keys are supplied")
}
