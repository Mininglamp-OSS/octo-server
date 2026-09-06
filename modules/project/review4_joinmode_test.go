package project

// Blocker 2 (yujiawei S-2): join_mode is client-writable and echoed on the wire, though
// plan.md:40 commits P0 to the opposite ("column kept, default 1, no consumer, same
// treatment as is_official"). is_official genuinely has no surface; join_mode is accepted
// in both request payloads, written through, and returned in every response — so a client
// can persist join_mode=0 today, and when P2 ships the self-join path every such row
// becomes open-join retroactively, with nobody re-consenting.
//
// The fix under test: full symmetry with is_official — the column stays (DDL default 1),
// and nothing above the storage layer knows the field exists.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJoinModeHasNoClientSurfaceInP0 pins the no-writer, no-reader contract.
func TestJoinModeHasNoClientSurfaceInP0(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerTok := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)

	// Create carrying join_mode=0: the field must not reach storage or the response.
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", ownerTok,
		map[string]any{"name": "jm", "join_mode": 0})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "join_mode",
		"the P0 response must not carry join_mode")

	// Update carrying join_mode=0: the field must be inert.
	created := decodeResp(t, w)
	w = doJSON(t, srv, http.MethodPut, "/v1/projects/"+created.ProjectID, ownerTok,
		map[string]any{"join_mode": 0})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "join_mode")

	// The detail view must not surface it either.
	w = doJSON(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID, ownerTok, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "join_mode")

	// And storage holds the DDL default, because nothing wrote the column.
	var jm int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT join_mode FROM `octo_project` WHERE project_id = ?", created.ProjectID).LoadOne(&jm))
	assert.Equal(t, JoinModeInviteOnly, jm,
		"join_mode must stay at its DDL default in P0; a persisted 0 would become "+
			"open-join semantics the day the P2 join path lands")
}
