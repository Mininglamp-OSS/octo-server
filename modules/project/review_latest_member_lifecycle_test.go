package project

import (
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemberAddUsesDirectoryBotEligibility keeps the personnel-management boundary
// separate from the agent target rules: only Owner/Admin may add, but an eligible
// directory bot is not restricted to the caller's own creator_uid.
func TestMemberAddUsesDirectoryBotEligibility(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "plain")

	seedUser(t, "other-owner")
	seedSpaceMember(t, spaceA, "other-owner", 0, 1)
	seedAgent(t, spaceA, "bot-directory", "other-owner", "octo_hosted")

	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("bot-directory"))
	require.Equal(t, http.StatusOK, w.Code, "an eligible directory bot may be added by the Project Owner: %s", w.Body.String())
	require.NotNil(t, memberRow(t, created.ProjectID, "bot-directory"))

	seedAgent(t, spaceA, "bot-self-hosted", "other-owner", "self_hosted")
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("bot-self-hosted"))
	assertProjectErrorCode(t, w, "err.server.project.agent_not_eligible")
	require.Nil(t, memberRow(t, created.ProjectID, "bot-self-hosted"),
		"an ineligible bot must not be admitted as a human")

	seedAgent(t, spaceA, "bot-plain", "plain", "octo_hosted")
	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", tokens["plain"],
		addMembersPayload("bot-plain"))
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")
	require.Nil(t, memberRow(t, created.ProjectID, "bot-plain"),
		"an ordinary member must not regain the own-bot personnel-management exception")
}

// TestOwnerTransferRejectsBotsAndCascadeCannotRemoveOwner covers both halves of the
// historical P1 chain: Owner transfer never grants Owner to a Bot, and a legacy
// bot-owner row cannot be swept away when its human creator leaves.
func TestOwnerTransferRejectsBotsAndCascadeCannotRemoveOwner(t *testing.T) {
	srv, p := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "leaver")
	seedAgent(t, spaceA, "bot-successor", "leaver", "octo_hosted")

	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		addMembersPayload("bot-successor"))
	require.Equal(t, http.StatusOK, w.Code, "eligible bot setup failed: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/owner", ownerToken,
		map[string]any{"uid": "bot-successor"})
	assertProjectErrorCode(t, w, "err.server.project.member_not_found")
	assert.Equal(t, RoleOwner, memberRow(t, created.ProjectID, "owner1").Role)
	assert.Equal(t, RoleCommon, memberRow(t, created.ProjectID, "bot-successor").Role)

	// Model the historical bad state produced by the pre-fix transfer path. The
	// leave itself remains a real user operation; only the impossible old role
	// assignment is seeded so the cascade protection is independently exercised.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET role = ?, updated_at = ? WHERE project_id = ? AND uid = ?",
		RoleOwner, time.Now().UTC(), created.ProjectID, "bot-successor").Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET role = ?, updated_at = ? WHERE project_id = ? AND uid = ?",
		RoleAdmin, time.Now().UTC(), created.ProjectID, "owner1").Exec()
	require.NoError(t, err)

	w = doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/leave", tokens["leaver"], nil)
	require.Equal(t, http.StatusOK, w.Code, "the human owner of the bot must still be able to leave: %s", w.Body.String())
	drainRemovalCascade(t, p)

	bot := memberRow(t, created.ProjectID, "bot-successor")
	require.NotNil(t, bot)
	assert.Equal(t, MemberStatusActive, bot.Status)
	assert.Zero(t, bot.Removing)
	assert.Equal(t, RoleOwner, bot.Role)
	assert.Equal(t, 1, activeOwnerCount(t, created.ProjectID),
		"an Owner seat must not be removed by the departing creator's cascade")
}

// TestOwnerlessScanTreatsRemovingOwnerAsMissing ensures a closing Owner is not
// counted as an authorized Owner during the two-phase removal window.
func TestOwnerlessScanTreatsRemovingOwnerAsMissing(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "keep")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET removing = 1, updated_at = ? WHERE project_id = ? AND uid = ?",
		time.Now().UTC(), created.ProjectID, "owner1").Exec()
	require.NoError(t, err)
	require.Equal(t, 0, activeOwnerCount(t, created.ProjectID), "precondition: the closing Owner is not active")

	resetCursorsForTest()
	p.scanOwnerlessProjects()
	assert.Equal(t, float64(1), testutil.ToFloat64(ownerlessProjects))
}

// TestI4ScanStopsAtThePageBoundAndCarriesItsCursor exercises the bounded
// incomplete-rotation path instead of pinning query source text. A later tick
// must resume after the last inspected Project rather than publish a partial
// gauge as if the whole population had been scanned.
func TestI4ScanStopsAtThePageBoundAndCarriesItsCursor(t *testing.T) {
	_, p := setup(t)
	seedSpace(t, spaceA, 1)
	now := time.Now().UTC()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project` (project_id, space_id, name, creator, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)",
		"i4-bound-1", spaceA, "i4-bound-1", "nobody", now, now,
		"i4-bound-2", spaceA, "i4-bound-2", "nobody", now, now,
	).Exec()
	require.NoError(t, err)

	oldLimit := p.cfg.ReconcileLimit
	p.cfg.ReconcileLimit = 1
	oldPages := reconcileMaxPagesForTest()
	setReconcileMaxPagesForTest(1)
	t.Cleanup(func() {
		p.cfg.ReconcileLimit = oldLimit
		setReconcileMaxPagesForTest(oldPages)
	})

	allMemberGroupMissing.Set(0)
	resetCursorsForTest()
	p.scanMissingAllMemberGroups()

	cursor, running := cursors.idResume(&cursors.i4Missing, &cursors.i4MissingRun)
	require.Positive(t, cursor)
	assert.Equal(t, 1, running,
		"the first missing project must be accumulated across the incomplete rotation")
	assert.Zero(t, testutil.ToFloat64(allMemberGroupMissing),
		"an incomplete bounded rotation must not publish a partial population gauge")
}
