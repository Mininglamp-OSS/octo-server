package space

// The manager doors' half of the space_id write rule.
//
// # Why this file exists separately, and what it says about the other guard
//
// TestEverySpaceActiveCheckRebindsTheSpaceID scans `s.checkSpaceActive` call sites. The
// manager handlers never call it — they load the row with querySpaceIncludeDisbanded
// instead — so that guard is blind to them BY CONSTRUCTION, and it stayed green while
// one manager door still stored the caller's URL parameter.
//
// Mutation-confirmed, by the reviewer and re-confirmed here: deleting a manager rebind
// leaves every other spelling test green. Nothing pinned those two rebinds, which is
// precisely how the third door was missed when the first two were fixed.
//
// The lesson this branch keeps relearning is that an instrument cannot fail on what it
// cannot express. The answer is not to widen the other guard's regexp — its scope is
// correct for what it checks — but to pin the manager doors by DRIVING them, which is
// the one form of coverage that does not depend on guessing which syntax a future door
// will use.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagerDoorsStoreTheSpaceRowsSpelling drives every manager door that writes a
// space_id with a drifted URL parameter and asserts the stored bytes are the row's.
//
// One test over a table of doors rather than one test each: the defect was "two of
// three doors fixed", so the shape that matters is a list the next door has to be added
// to, driven through the real route.
func TestManagerDoorsStoreTheSpaceRowsSpelling(t *testing.T) {
	s, _, err := setup(t)
	require.NoError(t, err)
	token := adminToken(t)

	doors := []struct {
		name string
		// spaceID is the canonical id seeded for this door.
		spaceID string
		// drive issues the request against the DRIFTED spelling.
		drive func(t *testing.T, driftedURLParam string)
		// stored reads back what the door wrote.
		stored func(t *testing.T, canonical string) []string
	}{
		{
			name:    "createInvite",
			spaceID: "mgrsid-invite",
			drive: func(t *testing.T, drifted string) {
				body := util.ToJson(map[string]interface{}{"max_uses": 5})
				w := httptest.NewRecorder()
				req, _ := http.NewRequest("POST",
					"/v1/manager/spaces/"+drifted+"/invites", bytes.NewReader([]byte(body)))
				req.Header.Set("token", token)
				s.GetRoute().ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			},
			stored: func(t *testing.T, canonical string) []string {
				return storedSpaceIDs(t, "SELECT space_id FROM space_invitation WHERE creator <> ''")
			},
		},
		{
			name:    "addMembers",
			spaceID: "mgrsid-add",
			drive: func(t *testing.T, drifted string) {
				body := util.ToJson(map[string]interface{}{"uids": []string{"mgrsid-u1"}})
				w := httptest.NewRecorder()
				req, _ := http.NewRequest("POST",
					"/v1/manager/spaces/"+drifted+"/members", bytes.NewReader([]byte(body)))
				req.Header.Set("token", token)
				s.GetRoute().ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			},
			stored: func(t *testing.T, canonical string) []string {
				return storedSpaceIDs(t,
					"SELECT space_id FROM space_member WHERE uid = 'mgrsid-u1'")
			},
		},
	}

	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			seedSpace(t, door.spaceID, door.name, "u-owner", SpaceStatusNormal)

			drifted := strings.ToUpper(door.spaceID)
			require.NotEqual(t, door.spaceID, drifted,
				"the fixture must differ from its drifted form or this case proves nothing")

			door.drive(t, drifted)

			got := door.stored(t, door.spaceID)
			require.NotEmpty(t, got, "the door must actually have written a row")
			for _, v := range got {
				assert.Equal(t, door.spaceID, v,
					"%s must store the space_id the ROW holds, not the URL parameter. The "+
						"manager handlers already load that row; keeping the parameter poisons "+
						"every seat and invitation derived from it, and a poisoned seat makes "+
						"the removal funnel hand its tx step drifted bytes — member_epoch "+
						"freezes while membership changes and the cascade completes as a "+
						"successful no-op.", door.name)
			}
		})
	}
}

// storedSpaceIDs is a small reader so each door's assertion reads as one line.
func storedSpaceIDs(t *testing.T, query string) []string {
	t.Helper()
	var out []string
	_, err := testCtx.DB().SelectBySql(query).Load(&out)
	require.NoError(t, err)
	return out
}
