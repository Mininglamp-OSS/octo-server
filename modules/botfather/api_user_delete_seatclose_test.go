package botfather

// D14 on the SECOND bot-deletion path.
//
// PR #855 routed the chat-command deletion through the Space removal outbox and
// left DELETE /v1/user/bots/:bot_id doing the bare `UPDATE space_member SET
// status=0`, so the same rule had two different answers depending on which door
// the owner used. The seventh review found it by running the census nobody had
// run — "which entry points can delete a bot" — and it is a residue with no
// witness: the bare update skips P0's project-seat cascade and P1's group detach,
// I1's scan is default-off (and gated on the collation conversion), I4 scan B
// looks for a member MISSING from the group while this ghost has both rows, and
// D13 can never reclaim the seat because it requires an active robot row.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seatCountFor(t *testing.T, ctx *config.Context, uid string, status int) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE uid = ? AND status = ?", uid, status,
	).LoadOne(&n))
	return n
}

func cleanupJobsFor(t *testing.T, ctx *config.Context, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member_removal_cleanup WHERE uid = ?", uid,
	).LoadOne(&n))
	return n
}

// TestRESTBotDeleteClosesSeatsThroughTheOutbox is the endpoint-level pin: after a
// REST delete, every Space seat is closed AND each one left a cleanup job behind.
//
// The job is the load-bearing half. Closing the seat is what the bare UPDATE
// already did; the job is what drives the project-seat cascade and the group
// detach, and its absence is the state no scan in this repository reports.
func TestRESTBotDeleteClosesSeatsThroughTheOutbox(t *testing.T) {
	route, ctx := newUserAPITestServer(t)
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	uid := "u_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "owner")
	token := mintUserAPIKey(t, ctx, uid)

	botID := "bot_" + util.GenerUUID()[:8]
	insertTestBot(t, ctx, botID, uid)

	// Two Spaces, so the assertion is about "every seat" rather than "a seat".
	for _, spaceID := range []string{"sp_rest_a", "sp_rest_b"} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
			spaceID, spaceID, uid).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
			spaceID, botID).Exec()
		require.NoError(t, err)
	}
	require.Equal(t, 2, seatCountFor(t, ctx, botID, 1), "precondition: the bot is seated")

	w := httptest.NewRecorder()
	route.ServeHTTP(w, userAPIRequest(t, http.MethodDelete,
		fmt.Sprintf("/v1/user/bots/%s", botID), token, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Zero(t, seatCountFor(t, ctx, botID, 1), "every Space seat must be closed")
	assert.Equal(t, 2, cleanupJobsFor(t, ctx, botID),
		"and each closed seat must leave a cleanup job — that job is what drives P0's "+
			"project-seat cascade and P1's group detach. A bare UPDATE closes the seat and "+
			"enqueues nothing, leaving a disabled bot holding an active project seat and an "+
			"active group_member row: I1's scan is default-off, I4 scan B looks for the "+
			"opposite state, and D13 cannot reclaim a seat whose robot row is disabled")
	assert.Equal(t, 0, robotStatusOf(t, ctx, botID), "the deletion itself still completes")
}
