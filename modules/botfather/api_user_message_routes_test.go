package botfather

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// The `message` table these reads hit is created by the webhook module's
	// migration, and a package test only runs migrations for the modules it
	// links (internal/modules.go is not in a test binary). Blank-import it so
	// the schema comes from the authoritative migration instead of a hand-copied
	// CREATE TABLE that would drift.
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
)

// ===========================================================================
// DM and thread single-message reads on the User API Key tree.
//
// The group-message shape (already covered by api_user_upstream_test.go) needs
// nothing but the actor. These two need more, and that is what these tests pin:
//
//   - DM is the only reused handler that reads a Space. Its isolation lives
//     behind pkg/space's GetSpaceID, which SpaceMiddleware fills on the human
//     route and nothing fills on this tree — hence authtree.BoundSpaceContext.
//   - the thread route only exists while DM_THREAD_ON is on, mirroring the
//     human route, so automation never sees a path humans do not have.
//
// One consequence of reusing personSpaceAllows verbatim, recorded here so the
// 404s it produces are not later read as a bug: that rule only lets an unlabelled
// DM message through when the request's Space is the caller's default one. So a
// key bound to a non-default Space cannot read DM messages predating Space
// labelling — it gets the same "not found" a person browsing that Space gets, and
// the fold to 404 is the anti-enumeration posture, not a distinct error. Anything
// that surfaces this to an end user should say "not in this Space", not "deleted".
// ===========================================================================

// messageShardTable mirrors message.DB.getTable: the message body lives in one of
// MessageTableCount shards picked by CRC32 of the channel id, so a fixture that
// writes to plain `message` is invisible to the handler for most channels.
func messageShardTable(ctx *config.Context, channelID string) string {
	count := ctx.GetConfig().TablePartitionConfig.MessageTableCount
	if count <= 0 {
		return "message"
	}
	idx := crc32.ChecksumIEEE([]byte(channelID)) % uint32(count)
	if idx == 0 {
		return "message"
	}
	return fmt.Sprintf("message%d", idx)
}

// insertMessageRow writes one message straight to its shard. The message module's
// insert helper is unexported and going through the IM would need a live send
// path; the read handlers only care about these columns.
func insertMessageRow(t *testing.T, ctx *config.Context, channelID string, channelType uint8, messageID int64, fromUID, payloadSpaceID string) {
	t.Helper()
	payload := map[string]any{"type": 1, "content": "hello"}
	if payloadSpaceID != "" {
		payload["space_id"] = payloadSpaceID
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO "+messageShardTable(ctx, channelID)+
			" (message_id, message_seq, client_msg_no, from_uid, channel_id, channel_type, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
		messageID, messageID, fmt.Sprintf("cli-%d", messageID), fromUID, channelID, channelType, raw,
	).Exec()
	require.NoError(t, err)
}

// insertTestGroupWithMember creates an active group whose only member is uid.
func insertTestGroupWithMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO `group` (group_no, name, creator, status, version) VALUES (?, ?, ?, 1, 1)",
		groupNo, groupNo, uid,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO group_member (group_no, uid, role, status, version, vercode) VALUES (?, ?, 2, 1, 1, ?)",
		groupNo, uid, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

// insertTestThreadRow creates an active thread under groupNo.
func insertTestThreadRow(t *testing.T, ctx *config.Context, groupNo, shortID, creatorUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO thread (short_id, group_no, name, creator_uid, status, version) VALUES (?, ?, 't', ?, 1, 1)",
		shortID, groupNo, creatorUID,
	).Exec()
	require.NoError(t, err)
}

// threadChannelID mirrors thread.BuildChannelID's four-underscore composite so
// this package does not have to import the thread module for one constant.
func threadChannelID(groupNo, shortID string) string {
	return groupNo + "____" + shortID
}

// newGroupNo returns a 32-hex group number, the only shape thread.IsValidGroupNo
// accepts.
func newGroupNo() string {
	return strings.ReplaceAll(util.GenerUUID(), "-", "")
}

// newShortID returns a 15-digit id, the shortest thread.IsValidShortID accepts.
var shortIDSeq atomic.Int64

func newShortID() string {
	return fmt.Sprintf("1%014d", shortIDSeq.Add(1))
}

// syncedMessage is the subset of message.MsgSyncResp these tests assert on.
type syncedMessage struct {
	MessageID   int64  `json:"message_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// TestUserKeyMessageRoutesAreMountedAndAuthGated is the credential matrix for the
// two new shapes: mounted for `uk_`, closed to bot tokens and session tokens.
func TestUserKeyMessageRoutesAreMountedAndAuthGated(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	ukToken := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	botID := "bot_" + util.GenerUUID()[:8]
	botToken := insertTestBot(t, ctx, botID, uid)

	groupNo := newGroupNo()
	paths := []string{
		"/v1/user/messages/person/peer_" + util.GenerUUID()[:8] + "/1",
		fmt.Sprintf("/v1/user/groups/%s/threads/%s/messages/1", groupNo, newShortID()),
	}

	for _, path := range paths {
		t.Run("uk_accepted "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, ukToken, nil))
			assert.NotEqual(t, http.StatusNotFound, w.Code,
				"route must be mounted on the uk tree: %s", w.Body.String())
			assert.NotEqual(t, http.StatusUnauthorized, w.Code,
				"a valid uk_ must pass authUserAPIKey: %s", w.Body.String())
		})
		t.Run("bf_rejected "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, botToken, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
		t.Run("session_rejected "+path, func(t *testing.T) {
			w := httptest.NewRecorder()
			route.ServeHTTP(w, userAPIRequest(t, http.MethodGet, path, testutil.Token, nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
	}
}

// TestUserKeyPersonMessageActorAndBoundSpace is the load-bearing DM test. One
// request proves three things at once:
//
//   - the actor is the person behind the key: the message is only reachable
//     through the fakeChannelID derived from that uid,
//   - the key's Space reaches the handler: caller and peer are Space colleagues
//     but NOT friends, so the same-space branch is the only way in,
//   - Space isolation is applied: a second message in the same DM channel but
//     labelled with another Space is refused.
//
// The third assertion is what fails if BoundSpaceContext is removed — an empty
// Space makes the handler skip the filter and return the cross-Space message.
func TestUserKeyPersonMessageActorAndBoundSpace(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	peer := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, peer, "peer")

	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	insertTestSpace(t, ctx, spaceID, peer)
	otherSpaceID := "sp_" + util.GenerUUID()[:8]

	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceID)
	channelID := common.GetFakeChannelIDWith(peer, owner)

	const inSpaceMsg int64 = 91001
	const crossSpaceMsg int64 = 91002
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), inSpaceMsg, peer, spaceID)
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), crossSpaceMsg, peer, otherSpaceID)

	get := func(messageID int64) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/user/messages/person/%s/%d", peer, messageID), token, nil))
		return w
	}

	w := get(inSpaceMsg)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got syncedMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, inSpaceMsg, got.MessageID)
	assert.Equal(t, channelID, got.ChannelID,
		"channel is derived from the key owner's uid, proving the actor is the person")
	assert.Equal(t, common.ChannelTypePerson.Uint8(), got.ChannelType)

	cross := get(crossSpaceMsg)
	assert.NotEqual(t, http.StatusOK, cross.Code,
		"a DM message labelled with another Space must not be readable: %s", cross.Body.String())
	assert.NotContains(t, cross.Body.String(), "hello",
		"the refusal must not leak the message body")
}

// insertFriendPair makes a and b mutual friends, the relationship that opens a DM
// without a shared Space.
func insertFriendPair(t *testing.T, ctx *config.Context, a, b string) {
	t.Helper()
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT IGNORE INTO friend (uid, to_uid, version, vercode) VALUES (?, ?, 1, ?)",
			pair[0], pair[1], util.GenerUUID(),
		).Exec()
		require.NoError(t, err)
	}
}

// TestUserKeyPersonMessageIsolatesCrossSpaceForFriends isolates the Space filter
// from the access gate. Caller and peer are friends, so the DM is open on
// relationship alone; the only thing that can still refuse a message is the
// per-message Space label. Drop BoundSpaceContext and this returns the other
// tenant's message with a 200.
func TestUserKeyPersonMessageIsolatesCrossSpaceForFriends(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	peer := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, peer, "peer")
	insertFriendPair(t, ctx, owner, peer)

	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	otherSpaceID := "sp_" + util.GenerUUID()[:8]

	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceID)
	channelID := common.GetFakeChannelIDWith(peer, owner)

	const mine int64 = 95001
	const theirs int64 = 95002
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), mine, peer, spaceID)
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), theirs, peer, otherSpaceID)

	get := func(messageID int64) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/user/messages/person/%s/%d", peer, messageID), token, nil))
		return w
	}

	ok := get(mine)
	require.Equal(t, http.StatusOK, ok.Code,
		"a friend's DM labelled with the key's Space must be readable: %s", ok.Body.String())

	cross := get(theirs)
	assert.NotEqual(t, http.StatusOK, cross.Code,
		"the same friendship must not carry another Space's message across: %s", cross.Body.String())
	assert.NotContains(t, cross.Body.String(), "hello")
}

// TestUserKeyPersonMessageSpaceUnboundKeyIsRefused pins the fail-closed posture
// for a key issued before Space binding existed (PR #713 review, Jerry-Xin
// blocker 3 / lml2468 P0).
//
// Such a key carries no Space, so there is no tenant to enforce and nothing for
// BoundSpaceContext to publish. Letting it through would hand the handler an empty
// Space, which takes the compatibility path that skips per-message Space filtering
// entirely — a credential whose stated guarantee is tenant confinement would then
// read a message labelled for another Space on the friendship alone. enforceKeySpace
// therefore refuses it with 403 before the handler runs.
//
// This is not a back-compat break: these upstream routes did not exist before this
// change, so no unbound key ever had access to lose. The space-bound key is refused
// on the same fixtures too, with the handler's own 404 — the two credentials differ
// only in binding, and neither can read across the boundary.
func TestUserKeyPersonMessageSpaceUnboundKeyIsRefused(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	peer := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, peer, "peer")
	insertFriendPair(t, ctx, owner, peer)

	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	otherSpaceID := "sp_" + util.GenerUUID()[:8]

	channelID := common.GetFakeChannelIDWith(peer, owner)
	const crossSpaceMsg int64 = 90101
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), crossSpaceMsg, peer, otherSpaceID)

	get := func(token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
			fmt.Sprintf("/v1/user/messages/person/%s/%d", peer, crossSpaceMsg), token, nil))
		return w
	}

	// Keys are stored per (uid, space, client) triple, so these are two distinct
	// credentials for the same person.
	unbound := get(mintUserAPIKey(t, ctx, owner))
	assert.Equal(t, http.StatusForbidden, unbound.Code,
		"a Space-unbound key has no enforceable tenant and must be refused: %s", unbound.Body.String())
	assert.NotContains(t, unbound.Body.String(), "hello")

	bound := get(mintUserAPIKeyInSpace(t, ctx, owner, spaceID))
	assert.NotEqual(t, http.StatusOK, bound.Code,
		"the same fixtures must stay refused for a key bound elsewhere: %s", bound.Body.String())
}

// TestUserKeyPersonMessageRejectsOtherPeoplesDM keeps the anti-enumeration
// posture: a DM the key owner is not part of stays invisible even when the
// caller names one of its participants.
func TestUserKeyPersonMessageRejectsOtherPeoplesDM(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	peerA := "u_" + util.GenerUUID()[:8]
	peerB := "u_" + util.GenerUUID()[:8]
	for _, u := range []string{owner, peerA, peerB} {
		insertTestUser(t, ctx, u, u)
	}
	spaceID := "sp_" + util.GenerUUID()[:8]
	for _, u := range []string{owner, peerA, peerB} {
		insertTestSpace(t, ctx, spaceID, u)
	}

	const mid int64 = 92001
	insertMessageRow(t, ctx, common.GetFakeChannelIDWith(peerA, peerB),
		common.ChannelTypePerson.Uint8(), mid, peerA, spaceID)

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		fmt.Sprintf("/v1/user/messages/person/%s/%d", peerB, mid),
		mintUserAPIKeyInSpace(t, ctx, owner, spaceID), nil))
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "hello")
}

// TestUserKeyThreadMessageReadsThread covers the happy path of the thread shape
// with the flag on, and its membership gate.
func TestUserKeyThreadMessageReadsThread(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceID)

	groupNo := newGroupNo()
	shortID := newShortID()
	insertTestGroupWithMember(t, ctx, groupNo, owner)
	insertTestThreadRow(t, ctx, groupNo, shortID, owner)

	const mid int64 = 93001
	insertMessageRow(t, ctx, threadChannelID(groupNo, shortID),
		common.ChannelTypeCommunityTopic.Uint8(), mid, owner, "")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		fmt.Sprintf("/v1/user/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid), token, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got syncedMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, mid, got.MessageID)
	assert.Equal(t, threadChannelID(groupNo, shortID), got.ChannelID)

	// A key whose owner is not in the parent group is refused, so the thread
	// route inherits the same membership gate as the human one.
	outsider := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, outsider, "outsider")
	insertTestSpace(t, ctx, spaceID, outsider)
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		fmt.Sprintf("/v1/user/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid),
		mintUserAPIKeyInSpace(t, ctx, outsider, spaceID), nil))
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
}

// TestUserKeyThreadMessageAbsentWhenThreadDisabled is the flag half: with
// DM_THREAD_ON off the human route is not registered either, so the uk route
// must be a 404 from the router — the handler never runs and no thread row is
// read. The DM route in the same group stays mounted, proving the guard is
// scoped to the thread shape.
func TestUserKeyThreadMessageAbsentWhenThreadDisabled(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "false")
	route, ctx := newUserAPITestServer(t)

	owner := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	token := mintUserAPIKeyInSpace(t, ctx, owner, spaceID)

	groupNo := newGroupNo()
	shortID := newShortID()
	insertTestGroupWithMember(t, ctx, groupNo, owner)
	insertTestThreadRow(t, ctx, groupNo, shortID, owner)
	const mid int64 = 94001
	insertMessageRow(t, ctx, threadChannelID(groupNo, shortID),
		common.ChannelTypeCommunityTopic.Uint8(), mid, owner, "")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		fmt.Sprintf("/v1/user/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid), token, nil))
	assert.Equal(t, http.StatusNotFound, w.Code,
		"the thread route must not exist while DM_THREAD_ON is off: %s", w.Body.String())

	// Same server, same tree: the group and DM shapes are unaffected.
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		fmt.Sprintf("/v1/user/groups/%s/messages/%d", groupNo, mid), token, nil))
	assert.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodGet,
		"/v1/user/messages/person/peer_x/1", token, nil))
	assert.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
}

// TestUserKeyPersonMessageMatchesHumanResponse locks the response-shape guard: the
// uk route mounts the human handler itself, with no wrapper and no envelope, so
// the same DM must serialise byte-identically on both trees. If a future change
// wraps or renames anything for automation, octo-drive's parser breaks and this
// fails first.
//
// The human side needs the Space in the header it normally carries; the uk side
// gets the same Space from the key. That the two agree is the point.
func TestUserKeyPersonMessageMatchesHumanResponse(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	owner := testutil.UID // the session token resolves to this uid
	peer := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, owner, "owner")
	insertTestUser(t, ctx, peer, "peer")

	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)
	insertTestSpace(t, ctx, spaceID, peer)

	channelID := common.GetFakeChannelIDWith(peer, owner)
	const mid int64 = 99001
	insertMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), mid, peer, spaceID)

	suffix := fmt.Sprintf("/messages/person/%s/%d", peer, mid)
	humanReq := httptest.NewRequest(http.MethodGet, "/v1"+suffix+"?space_id="+spaceID, nil)
	humanReq.Header.Set("token", testutil.Token)
	human := httptest.NewRecorder()
	route.ServeHTTP(human, humanReq)
	require.Equal(t, http.StatusOK, human.Code, human.Body.String())

	uk := httptest.NewRecorder()
	route.ServeHTTP(uk, userAPIRequest(t, http.MethodGet, "/v1/user"+suffix,
		mintUserAPIKeyInSpace(t, ctx, owner, spaceID), nil))
	require.Equal(t, http.StatusOK, uk.Code, uk.Body.String())

	assert.JSONEq(t, human.Body.String(), uk.Body.String(),
		"the uk tree must return the human handler's response verbatim")
}

// TestUserKeyThreadMessageMatchesHumanResponse is the response-shape guard for the
// thread shape, the DM one's counterpart. getThreadMessage reads no Space, so the
// two requests differ in nothing but the credential and the path prefix — which is
// exactly what makes any divergence a wrapper or a renamed field.
func TestUserKeyThreadMessageMatchesHumanResponse(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUserAPITestServer(t)

	owner := testutil.UID // the session token resolves to this uid
	insertTestUser(t, ctx, owner, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, owner)

	groupNo := newGroupNo()
	shortID := newShortID()
	insertTestGroupWithMember(t, ctx, groupNo, owner)
	insertTestThreadRow(t, ctx, groupNo, shortID, owner)

	const mid int64 = 99101
	insertMessageRow(t, ctx, threadChannelID(groupNo, shortID),
		common.ChannelTypeCommunityTopic.Uint8(), mid, owner, "")

	suffix := fmt.Sprintf("/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid)
	humanReq := httptest.NewRequest(http.MethodGet, "/v1"+suffix, nil)
	humanReq.Header.Set("token", testutil.Token)
	human := httptest.NewRecorder()
	route.ServeHTTP(human, humanReq)
	require.Equal(t, http.StatusOK, human.Code, human.Body.String())

	uk := httptest.NewRecorder()
	route.ServeHTTP(uk, userAPIRequest(t, http.MethodGet, "/v1/user"+suffix,
		mintUserAPIKeyInSpace(t, ctx, owner, spaceID), nil))
	require.Equal(t, http.StatusOK, uk.Code, uk.Body.String())

	assert.JSONEq(t, human.Body.String(), uk.Body.String(),
		"the uk tree must return the human handler's response verbatim")
}

// TestUserKeyMessagesSubtreeCoexists is the zero-regression guard for the
// /v1/user/messages prefix: module setup must not panic once a parameterised
// route joins the search subtree under the same segment, and both must still be
// reachable over HTTP.
//
// It is deliberately NOT a same-method-tree shadowing test: the search subtree
// contributes POST endpoints only, the new route is a GET, and Gin keeps one
// radix tree per method — so nothing here can shadow anything. What this pins is
// registration and reachability, which is where a future sibling (a GET added to
// the search subtree, say) would first break.
func TestUserKeyMessagesSubtreeCoexists(t *testing.T) {
	route, ctx := newUserAPITestServer(t)

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	spaceID := "sp_" + util.GenerUUID()[:8]
	insertTestSpace(t, ctx, spaceID, uid)
	token := mintUserAPIKeyInSpace(t, ctx, uid, spaceID)

	search := httptest.NewRecorder()
	route.ServeHTTP(search, userAPIRequest(t, http.MethodPost,
		"/v1/user/messages/_search", token, map[string]any{"keyword": "x"}))
	assert.NotEqual(t, http.StatusNotFound, search.Code,
		"the search subtree must stay registered next to the new sibling: %s", search.Body.String())

	person := httptest.NewRecorder()
	route.ServeHTTP(person, userAPIRequest(t, http.MethodGet,
		"/v1/user/messages/person/peer_x/1", token, nil))
	assert.NotEqual(t, http.StatusNotFound, person.Code,
		"the DM route must be reachable next to the search subtree: %s", person.Body.String())
}
