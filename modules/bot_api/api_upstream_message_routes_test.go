package bot_api

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
	// the schema comes from the authoritative migration.
	_ "github.com/Mininglamp-OSS/octo-server/modules/webhook"
)

// ===========================================================================
// DM and thread single-message reads on the bot-token tree.
//
// The bot tree deliberately withholds a group-wide alias uid, so the actor only
// exists on the authtree mount (botActorUID). Every assertion below is about
// what that actor buys and — more importantly — what it does not: a bot has no
// space_member row, so it can never take the same-Space route into a DM. Its
// only way in is a relationship it actually holds.
// ===========================================================================

func botMessageShardTable(ctx *config.Context, channelID string) string {
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

func insertBotMessageRow(t *testing.T, ctx *config.Context, channelID string, channelType uint8, messageID int64, fromUID string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": 1, "content": "hello"})
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO "+botMessageShardTable(ctx, channelID)+
			" (message_id, message_seq, client_msg_no, from_uid, channel_id, channel_type, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
		messageID, messageID, fmt.Sprintf("cli-%d", messageID), fromUID, channelID, channelType, raw,
	).Exec()
	require.NoError(t, err)
}

// insertBotFriendPair mirrors what bot creation does: owner and bot become mutual
// friends, which is what opens their DM.
func insertBotFriendPair(t *testing.T, ctx *config.Context, a, b string) {
	t.Helper()
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT IGNORE INTO friend (uid, to_uid, version, vercode) VALUES (?, ?, 1, ?)",
			pair[0], pair[1], util.GenerUUID(),
		).Exec()
		require.NoError(t, err)
	}
}

func insertBotGroupWithMember(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO `group` (group_no, name, creator, status, version) VALUES (?, ?, ?, 1, 1)",
		groupNo, groupNo, uid,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO group_member (group_no, uid, role, status, version, vercode, robot) VALUES (?, ?, 0, 1, 1, ?, 1)",
		groupNo, uid, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

func insertBotThreadRow(t *testing.T, ctx *config.Context, groupNo, shortID, creatorUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO thread (short_id, group_no, name, creator_uid, status, version) VALUES (?, ?, 't', ?, 1, 1)",
		shortID, groupNo, creatorUID,
	).Exec()
	require.NoError(t, err)
}

func botThreadChannelID(groupNo, shortID string) string { return groupNo + "____" + shortID }

func newBotGroupNo() string { return strings.ReplaceAll(util.GenerUUID(), "-", "") }

var botShortIDSeq atomic.Int64

func newBotShortID() string { return fmt.Sprintf("2%014d", botShortIDSeq.Add(1)) }

type botSyncedMessage struct {
	MessageID int64  `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

// TestBotMessageRoutesAreMountedAndAuthGated is the credential matrix for the two
// new bot-tree shapes.
func TestBotMessageRoutesAreMountedAndAuthGated(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)

	paths := []string{
		"/v1/bot/messages/person/peer_" + util.GenerUUID()[:8] + "/1",
		fmt.Sprintf("/v1/bot/groups/%s/threads/%s/messages/1", newBotGroupNo(), newBotShortID()),
	}

	for _, path := range paths {
		w := upstreamGet(t, route, path, token)
		require.NotEqual(t, http.StatusNotFound, w.Code,
			"route must be mounted on the bot tree: %s %s", path, w.Body.String())
		require.NotEqual(t, http.StatusUnauthorized, w.Code,
			"a valid bf_ must pass authBot: %s %s", path, w.Body.String())

		uk := upstreamGet(t, route, path, "uk_"+util.GenerUUID())
		assert.Equal(t, http.StatusUnauthorized, uk.Code,
			"a user key must not authenticate on the bot tree: %s %s", path, uk.Body.String())

		// A browser session token is the third credential kind and must not open
		// the bot tree either — authBot only accepts bf_/app_.
		session := upstreamGet(t, route, path, testutil.Token)
		assert.Equal(t, http.StatusUnauthorized, session.Code,
			"a session token must not authenticate on the bot tree: %s %s", path, session.Body.String())
	}
}

// TestBotPersonMessageActorIsTheBot is the §3.3 proof. The DM row only exists in
// the channel derived from the bot's own robot_id, so reading it back means the
// actor botActorUID installed is the bot — not an empty uid and not the owner.
// The bot reaches it through the friendship its creation established, which is
// the only door open to it: it has no space_member row, so the same-Space branch
// is structurally unavailable.
func TestBotPersonMessageActorIsTheBot(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)
	owner := "u_" + util.GenerUUID()[:8]
	addSpaceMember(t, ctx, "sp_"+util.GenerUUID()[:8], owner)
	insertBotFriendPair(t, ctx, botID, owner)

	channelID := common.GetFakeChannelIDWith(owner, botID)
	const mid int64 = 96001
	insertBotMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), mid, owner)

	w := upstreamGet(t, route, fmt.Sprintf("/v1/bot/messages/person/%s/%d", owner, mid), token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got botSyncedMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, mid, got.MessageID)
	assert.Equal(t, channelID, got.ChannelID,
		"the channel is derived from the caller's uid, so this pins actor == robot_id")
}

// TestBotPersonMessageWithoutRelationshipIsRefused documents the posture the bot
// tree relies on instead of a Space: with no friendship and no space_member row
// the bot cannot read a DM even when it names a real peer. This is what keeps the
// empty-Space compatibility path from being a hole on this tree.
func TestBotPersonMessageWithoutRelationshipIsRefused(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)
	stranger := "u_" + util.GenerUUID()[:8]
	spaceID := "sp_" + util.GenerUUID()[:8]
	// Both sit in the same Space on purpose: a bot still must not inherit
	// same-Space DM access from it.
	addSpaceMember(t, ctx, spaceID, stranger)
	addSpaceMember(t, ctx, spaceID, botID)

	channelID := common.GetFakeChannelIDWith(stranger, botID)
	const mid int64 = 96002
	insertBotMessageRow(t, ctx, channelID, common.ChannelTypePerson.Uint8(), mid, stranger)

	w := upstreamGet(t, route, fmt.Sprintf("/v1/bot/messages/person/%s/%d", stranger, mid), token)
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "hello")
}

// TestBotPersonMessageRejectsOtherPeoplesDM keeps a bot out of DMs it is not a
// party to, even between two of its own group's members.
func TestBotPersonMessageRejectsOtherPeoplesDM(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)
	peerA := "u_" + util.GenerUUID()[:8]
	peerB := "u_" + util.GenerUUID()[:8]
	spaceID := "sp_" + util.GenerUUID()[:8]
	addSpaceMember(t, ctx, spaceID, peerA)
	addSpaceMember(t, ctx, spaceID, peerB)
	// Even a friendship with one participant must not expose their DM with a third.
	insertBotFriendPair(t, ctx, botID, peerB)

	const mid int64 = 96003
	insertBotMessageRow(t, ctx, common.GetFakeChannelIDWith(peerA, peerB),
		common.ChannelTypePerson.Uint8(), mid, peerA)

	w := upstreamGet(t, route, fmt.Sprintf("/v1/bot/messages/person/%s/%d", peerB, mid), token)
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "hello")
}

// TestBotThreadMessageReadsThread covers the thread shape with the flag on, plus
// its parent-group membership gate.
func TestBotThreadMessageReadsThread(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "true")
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)

	groupNo := newBotGroupNo()
	shortID := newBotShortID()
	insertBotGroupWithMember(t, ctx, groupNo, botID)
	insertBotThreadRow(t, ctx, groupNo, shortID, botID)

	channelID := botThreadChannelID(groupNo, shortID)
	const mid int64 = 97001
	insertBotMessageRow(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid, botID)

	w := upstreamGet(t, route,
		fmt.Sprintf("/v1/bot/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid), token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got botSyncedMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, mid, got.MessageID)
	assert.Equal(t, channelID, got.ChannelID)

	// A bot outside the parent group is refused, so the thread read inherits the
	// same membership gate as the human route.
	outsider := "bot_" + util.GenerUUID()[:8]
	outsiderToken := insertUpstreamBot(t, ctx, outsider)
	w = upstreamGet(t, route,
		fmt.Sprintf("/v1/bot/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid), outsiderToken)
	assert.NotEqual(t, http.StatusOK, w.Code, w.Body.String())
}

// TestBotThreadMessageAbsentWhenThreadDisabled is the flag half on this tree: no
// route at all, so nothing reads a thread row. The DM and group shapes stay up.
func TestBotThreadMessageAbsentWhenThreadDisabled(t *testing.T) {
	t.Setenv("DM_THREAD_ON", "false")
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)

	groupNo := newBotGroupNo()
	shortID := newBotShortID()
	insertBotGroupWithMember(t, ctx, groupNo, botID)
	insertBotThreadRow(t, ctx, groupNo, shortID, botID)
	const mid int64 = 98001
	insertBotMessageRow(t, ctx, botThreadChannelID(groupNo, shortID),
		common.ChannelTypeCommunityTopic.Uint8(), mid, botID)

	w := upstreamGet(t, route,
		fmt.Sprintf("/v1/bot/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid), token)
	assert.Equal(t, http.StatusNotFound, w.Code,
		"the thread route must not exist while DM_THREAD_ON is off: %s", w.Body.String())

	w = upstreamGet(t, route, fmt.Sprintf("/v1/bot/groups/%s/messages/%d", groupNo, mid), token)
	assert.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	w = upstreamGet(t, route, "/v1/bot/messages/person/peer_x/1", token)
	assert.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
}

// TestBotMessagesSubtreeCoexists is the bot-side zero-regression guard for the
// /v1/bot/messages prefix: setup must not panic with a parameterised route under
// the same segment as the search subtree, and both must stay reachable. Not a
// shadowing test — the search subtree is POST-only and this route is a GET, and
// Gin keeps one tree per method.
func TestBotMessagesSubtreeCoexists(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)

	search := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/bot/messages/_search",
		strings.NewReader(`{"keyword":"x"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	route.ServeHTTP(search, req)
	assert.NotEqual(t, http.StatusNotFound, search.Code,
		"the search subtree must stay registered next to the new sibling: %s", search.Body.String())

	person := upstreamGet(t, route, "/v1/bot/messages/person/peer_x/1", token)
	assert.NotEqual(t, http.StatusNotFound, person.Code,
		"the DM route must be reachable next to the search subtree: %s", person.Body.String())
}
