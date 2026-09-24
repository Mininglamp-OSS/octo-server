package botfather

// REST Bot deletion must close each Space seat through the removal outbox
// before disabling the Bot, so Project and native group cleanup can finish.

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
		"each closed seat must enqueue native group and Project seat cleanup")
	assert.Equal(t, 0, robotStatusOf(t, ctx, botID), "the deletion itself still completes")
}
