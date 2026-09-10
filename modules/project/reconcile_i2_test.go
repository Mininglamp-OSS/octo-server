package project

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// readGauge reads a gauge's current value. The repo's existing precedent is
// modules/file's use of prometheus/testutil for the same purpose.
func readGauge(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	return promtestutil.ToFloat64(g)
}

// The I3 attribution and removal-stall reconcile scans, exercised against a
// real MySQL 8.0.
//
// I3 checks that each live group relation still points at a live Project in
// the same Space. The stall scan covers the separate case where a Project
// seat remains in its closing state beyond the worker threshold.

// seedProjectGroup inserts a group attributed to a project, bypassing every
// admission path on purpose: this fixture builds the state a violation LOOKS
// like, which the funnel is designed to make unreachable.
func seedProjectGroup(t *testing.T, groupNo, spaceID, projectID string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status, `version`, space_id, project_id) "+
			"VALUES (?, ?, '', 1, 1, ?, ?)",
		groupNo, "g-"+groupNo, spaceID, projectID,
	).Exec()
	require.NoError(t, err)
}

// TestI3ScanReportsBrokenAttribution covers all three I3 shapes at once:
// disbanded, cross-Space, and absent.
func TestI3ScanReportsBrokenAttribution(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedSpaceMember(t, spaceB, "owner1", 0, 1)

	live := createProjectVia(t, srv, spaceA, token, "i3-live")
	otherSpace := createProjectVia(t, srv, spaceB, token, "i3-other")
	disbanded := createProjectVia(t, srv, spaceA, token, "i3-dead")
	_, err := p.disbandProject(disbanded.ProjectID, "owner1", spaceA)
	require.NoError(t, err)

	// Healthy: must not be reported.
	seedProjectGroup(t, util.GenerUUID(), spaceA, live.ProjectID)
	// Disbanded project.
	seedProjectGroup(t, util.GenerUUID(), spaceA, disbanded.ProjectID)
	// A project that lives in another Space.
	seedProjectGroup(t, util.GenerUUID(), spaceA, otherSpace.ProjectID)
	// A project id that does not exist at all.
	seedProjectGroup(t, util.GenerUUID(), spaceA, util.GenerUUID())

	resetCursorsForTest()
	p.scanI3Violations()
	require.Equal(t, 3, int(readGauge(t, i3Violations)),
		"disbanded, cross-Space and missing attribution must each be reported; a live one must not")
}

// TestRemovingStallReportsAClosingSeat pins the worker-stall signal independently
// from the I3 attribution scan.
func TestRemovingStallReportsAClosingSeat(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "stuck")
	seedSpaceMember(t, spaceA, "stuck", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "stall")

	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "stuck")
	require.NoError(t, err)
	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "stuck")
	require.NoError(t, err)

	// Not stalled yet: within the threshold, nothing is reported.
	p.scanRemovingStalls()
	require.Zero(t, int(readGauge(t, removingStalls)),
		"a removal that just started is not a stall")

	// Age the seat past the threshold.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `octo_project_member` SET updated_at = ? WHERE project_id = ? AND uid = ?",
		time.Now().UTC().Add(-2*removingStallAfter), created.ProjectID, "stuck",
	).Exec()
	require.NoError(t, err)

	p.scanRemovingStalls()
	require.Equal(t, 1, int(readGauge(t, removingStalls)), "a seat stuck past the threshold must be reported")

}
