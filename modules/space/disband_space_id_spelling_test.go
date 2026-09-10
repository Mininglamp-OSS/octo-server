package space

// The removal-outbox half of the space_id write rule.
//
// # What was wrong, and why nothing caught it
//
// space_member_removal_cleanup rows carry the pair the async cascade matches on:
// (space_id, uid). The uid side has been canonical since the seat funnel landed —
// lockActiveMemberUIDsTx reads it out of space_member. The space_id side was the
// caller's URL parameter, on BOTH disband doors, because they are the only
// removal-outbox producers with no seat to resolve and so took a plain string.
//
// A drifted parameter passes every gate on the way in (space, space_member and the
// Space middleware are all utf8mb4_0900_ai_ci in production), lands on N outbox rows,
// and is then matched against octo_project_member — pinned utf8mb4_general_ci, which
// does NOT fold the fullwidth forms 0900_ai_ci just folded. The worker enumerates
// zero project seats, marks the job done, and every project seat in that Space stays
// status = 1 permanently with the I1 scan reporting a violation nothing repairs.
//
// Both existing spelling guards are blind to these two doors BY CONSTRUCTION:
// TestEverySpaceActiveCheckRebindsTheSpaceID scans checkSpaceActive call sites and
// disbandSpace has none (it authorizes with queryMember instead), and
// TestManagerDoorsStoreTheSpaceRowsSpelling drives a table that forceDisband is not
// in. That is the fourth time on this branch an instrument reported a door correct
// because it could not express it, so the fix is a TYPE — enqueueMemberRemovalCleanup-
// BatchTx now takes a SpaceRef and a raw string does not compile — and this file
// drives the two doors to pin that the resolution happens against the right row.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDisbandDoorsStoreTheSpaceRowsSpellingInTheRemovalOutbox drives each disband
// door with a drifted URL parameter and asserts the outbox rows carry the row's.
func TestDisbandDoorsStoreTheSpaceRowsSpellingInTheRemovalOutbox(t *testing.T) {
	cases := []struct {
		name    string
		spaceID string
		owner   string
		path    func(drifted string) string
		token   func(t *testing.T, owner string) string
	}{
		{
			name:    "disbandSpace",
			spaceID: "disband-user-sid",
			owner:   "disband-owner",
			path:    func(drifted string) string { return "/v1/space/" + drifted },
			token:   func(t *testing.T, owner string) string { return joinApplicantToken(t, owner) },
		},
		{
			name:    "forceDisband",
			spaceID: "disband-mgr-sid",
			owner:   "disband-mgr-owner",
			path:    func(drifted string) string { return "/v1/manager/spaces/" + drifted },
			token:   func(t *testing.T, _ string) string { return superAdminToken(t) },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, err := setup(t)
			require.NoError(t, err)

			seedSpace(t, c.spaceID, c.name, c.owner, SpaceStatusNormal)
			// A second member, so the outbox is a batch rather than a single row —
			// the batch path is the one both doors take and the one that was wrong.
			require.NoError(t, testSpaceDB.insertMemberNoTx(&MemberModel{
				SpaceId: c.spaceID, UID: c.owner + "-mate", Role: 0, Status: 1,
			}))

			drifted := strings.ToUpper(c.spaceID)
			require.NotEqual(t, c.spaceID, drifted,
				"the fixture must differ from its drifted form or this case proves nothing")

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("DELETE", c.path(drifted), nil)
			req.Header.Set("token", c.token(t, c.owner))
			srv.GetRoute().ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			var stored []string
			_, err = testCtx.DB().SelectBySql(
				"SELECT space_id FROM space_member_removal_cleanup").Load(&stored)
			require.NoError(t, err)
			require.Len(t, stored, 2,
				"the door must have enqueued one cleanup job per active member; %d rows "+
					"means the fixture stopped exercising the batch path", len(stored))
			for _, got := range stored {
				assert.Equal(t, c.spaceID, got,
					"%s must write the space_id the `space` ROW holds into the removal "+
						"outbox, not the URL parameter.\n\n"+
						"The uid on these rows is already canonical (lockActiveMemberUIDsTx "+
						"reads it from space_member), and the async cascade matches the PAIR "+
						"against octo_project_member under utf8mb4_general_ci. A drifted "+
						"space_id there enumerates zero project seats, so the job completes "+
						"as a successful no-op and every project seat in this Space stays "+
						"status = 1 forever.", c.name)
			}
		})
	}
}
