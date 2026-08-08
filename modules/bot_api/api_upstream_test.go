package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUpstreamTestServer builds a server with the i18n renderer wired, the same way
// the other bot_api integration tests do.
func newUpstreamTestServer(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	s.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	return s.GetRoute(), ctx
}

// insertUpstreamBot registers an active bot and returns its bot token.
func insertUpstreamBot(t *testing.T, ctx *config.Context, robotID string) string {
	t.Helper()
	token := "bf_" + robotID
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO robot (robot_id, username, token, version, status, creator_uid, bot_token, im_token_cache, bot_commands) VALUES (?, ?, 'test_token', 1, 1, ?, ?, '', '[]')",
		robotID, robotID, "owner_"+robotID, token,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO user (uid, name, username, status, robot, short_no, zone, phone) VALUES (?, ?, ?, 1, 1, ?, '', '')",
		robotID, robotID, robotID, util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	return token
}

// addSpaceMember creates the Space (idempotently) and adds uid as an active member.
func addSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space (space_id, name, status) VALUES (?, ?, 1)", spaceID, spaceID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
	// A member row alone is not enough for the member list to name anyone — the
	// SELECT joins `user` for the display name.
	_, err = ctx.DB().InsertBySql(
		"INSERT IGNORE INTO user (uid, name, username, status, short_no, zone, phone) VALUES (?, ?, ?, 1, ?, '', '')",
		uid, uid, uid, util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
}

func upstreamGet(t *testing.T, route http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	return w
}

func decodeMemberUIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var members []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))
	uids := make([]string, 0, len(members))
	for _, m := range members {
		uids = append(uids, m["uid"].(string))
	}
	return uids
}

// ===========================================================================
// G12 — /v1/bot/space/members pagination.
//
// Before this, the endpoint capped at `limit` with no way to advance, so a bot in
// a Space larger than the cap could never enumerate past the first page: drive's
// IsSpaceMember could not tell "not a member" from "truncated" and fail-closed
// into a 500. The compatibility floor is that a request WITHOUT `page` behaves
// exactly as it did before.
// ===========================================================================

func TestBotSpaceMembersPagination(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)
	spaceID := "sp_" + util.GenerUUID()[:8]
	addSpaceMember(t, ctx, spaceID, botID)

	// Six members total (bot + 5 people) so limit=2 yields three full pages.
	for i := 0; i < 5; i++ {
		addSpaceMember(t, ctx, spaceID, "u_"+util.GenerUUID()[:8])
	}

	all := decodeMemberUIDs(t, upstreamGet(t, route, "/v1/bot/space/members?space_id="+spaceID+"&limit=50", token))
	require.Len(t, all, 6)

	t.Run("no page is the first page", func(t *testing.T) {
		withoutPage := decodeMemberUIDs(t, upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&limit=2", token))
		withPageOne := decodeMemberUIDs(t, upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&limit=2&page=1", token))
		assert.Len(t, withoutPage, 2)
		assert.Equal(t, withoutPage, withPageOne,
			"omitting page must be identical to page=1 — this is the compatibility floor")
		assert.Equal(t, all[:2], withoutPage, "the first page must be the head of the full list")
	})

	t.Run("page 2 advances without overlap", func(t *testing.T) {
		page1 := decodeMemberUIDs(t, upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&limit=2&page=1", token))
		page2 := decodeMemberUIDs(t, upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&limit=2&page=2", token))
		page3 := decodeMemberUIDs(t, upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&limit=2&page=3", token))

		require.Len(t, page2, 2)
		require.Len(t, page3, 2)
		for _, uid := range page2 {
			assert.NotContains(t, page1, uid, "page 2 must not repeat page 1")
		}
		// Walking every page must reconstruct the full list exactly — that is what
		// lets a caller distinguish "absent" from "truncated".
		walked := append(append([]string{}, page1...), append(page2, page3...)...)
		assert.ElementsMatch(t, all, walked)
	})

	t.Run("past the last page is empty not an error", func(t *testing.T) {
		w := upstreamGet(t, route, "/v1/bot/space/members?space_id="+spaceID+"&limit=2&page=99", token)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.JSONEq(t, "[]", w.Body.String())
	})

	t.Run("keyword search paginates too", func(t *testing.T) {
		// All seeded people share no name prefix, so search by the bot's own name
		// is enough to prove the keyword branch honours page/offset.
		page2 := upstreamGet(t, route,
			"/v1/bot/space/members?space_id="+spaceID+"&keyword="+botID+"&limit=1&page=2", token)
		require.Equal(t, http.StatusOK, page2.Code, page2.Body.String())
		assert.JSONEq(t, "[]", page2.Body.String(),
			"only one member matches, so its second page must be empty")
	})

	t.Run("non-member bot is still refused", func(t *testing.T) {
		outsider := "bot_" + util.GenerUUID()[:8]
		outsiderToken := insertUpstreamBot(t, ctx, outsider)
		w := upstreamGet(t, route, "/v1/bot/space/members?space_id="+spaceID+"&page=2", outsiderToken)
		assert.NotEqual(t, http.StatusOK, w.Code,
			"pagination must not bypass the bot-is-a-member check: %s", w.Body.String())
	})
}

// ===========================================================================
// G9 — GET /v1/bot/groups/:group_no/messages/:message_id on the bot-token tree.
// ===========================================================================

// TestBotGroupMessageRouteIsMountedAndAuthGated proves the route exists on the bot
// tree (a 404 would mean the authtree contribution never mounted) and that the
// user-key credential does not open it — the two trees stay separate doors.
func TestBotGroupMessageRouteIsMountedAndAuthGated(t *testing.T) {
	route, ctx := newUpstreamTestServer(t)

	botID := "bot_" + util.GenerUUID()[:8]
	token := insertUpstreamBot(t, ctx, botID)
	groupNo := "g" + util.GenerUUID()[:12]
	path := "/v1/bot/groups/" + groupNo + "/messages/1"

	w := upstreamGet(t, route, path, token)
	require.NotEqual(t, http.StatusNotFound, w.Code,
		"route must be mounted on the bot tree: %s", w.Body.String())
	require.NotEqual(t, http.StatusUnauthorized, w.Code,
		"a valid bf_ must pass authBot: %s", w.Body.String())
	// The bot is not a member of the group, so it must be refused — not 500'd.
	// This is also the proof that botActorUID landed: with an empty actor the
	// membership check would fail differently (and GetLoginUID would be blank).
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound}, w.Code,
		"a non-member bot must get a clean refusal: %d %s", w.Code, w.Body.String())

	// A uk_ token must not authenticate on the bot tree.
	uk := upstreamGet(t, route, path, "uk_"+util.GenerUUID())
	assert.Equal(t, http.StatusUnauthorized, uk.Code, uk.Body.String())
}
