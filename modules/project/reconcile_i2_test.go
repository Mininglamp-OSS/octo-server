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

// The I2 / I3 reconcile scans, exercised against a real MySQL 8.0.
//
// These cases are not only about counting. Every query they drive JOINs
// `group` / `group_member` (created in 2019 with no explicit COLLATE, inheriting
// the server default) against octo_project* (pinned utf8mb4_general_ci). P0's
// round-4 verification measured PRODUCTION at utf8mb4_0900_ai_ci on the legacy
// side, so an implicit join is MySQL error 1267 there while passing in CI. A
// scan that errors in production reports zero violations and looks healthy, so
// "the scan ran without error" is itself one of the assertions here.

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

// seedGroupMemberRow writes an active group_member row directly. Same reason:
// the funnel would refuse this, which is the point of seeding it in SQL.
func seedGroupMemberRow(t *testing.T, groupNo, uid string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, `version`, is_deleted, status, vercode, robot, invite_uid) "+
			"VALUES (?, ?, 0, 1, 0, 1, ?, 0, '')",
		groupNo, uid, util.GenerUUID(),
	).Exec()
	require.NoError(t, err)
}

func i2Count(t *testing.T, p *Project) int {
	t.Helper()
	resetCursorsForTest()
	p.scanI2Violations()
	// The gauge publishes only on a completed rotation, which a test-sized table
	// always is.
	return int(readGauge(t, i2Violations))
}

// TestI2ScanReportsASeededViolation seeds exactly the state the admission funnel
// exists to prevent — an active member of a project group who holds no project
// seat — and asserts the scan reports it.
//
// Seeded in raw SQL rather than through an endpoint because there is no endpoint
// that can produce it. That is what makes this the scan's real test: if the
// funnel is doing its job, the ONLY way to observe a violation is to write one
// directly, and a scan nobody can trigger is a scan nobody has tested.
func TestI2ScanReportsASeededViolation(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "intruder")
	seedSpaceMember(t, spaceA, "intruder", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "i2-scan")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "intruder")

	require.Equal(t, 1, i2Count(t, p),
		"a member of a project group who holds no project seat must be reported")
}

// TestI2ScanIgnoresAnActualProjectMember is the negative half: the same shape,
// but the member legitimately holds a seat. A scan that reports this would page
// on every healthy project group in production.
func TestI2ScanIgnoresAnActualProjectMember(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "i2-clean")

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "owner1") // the project's owner

	require.Zero(t, i2Count(t, p), "an active project member must not be reported")
}

// TestI2ScanExemptsSeatsWithAPendingCascade covers exemption 1, which is the one
// normal operation produces constantly.
//
// Between "a member was removed from a project" and "the worker finished
// detaching their groups", their group_member rows legitimately outlive their
// seat — that is D4's designed intermediate state, and the whole reason removal
// keeps status = 1 while removing = 1. Without this exemption the scan would
// alert on every single removal, and an alert that fires on normal operation is
// noise before the feature has a user.
func TestI2ScanExemptsSeatsWithAPendingCascade(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	seedUser(t, "leaver")
	seedSpaceMember(t, spaceA, "leaver", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "i2-exempt")

	_, err := p.addOneMember(created.ProjectID, spaceA, "owner1", "leaver")
	require.NoError(t, err)

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	seedGroupMemberRow(t, groupNo, "leaver")
	require.Zero(t, i2Count(t, p), "a healthy member must not be reported")

	// Begin removal. The seat is now removing=1 with a pending job, and the
	// group row is still there — the exempt state.
	_, err = p.removeMember(created.ProjectID, spaceA, "owner1", "leaver")
	require.NoError(t, err)

	pending, err := p.db.countPendingRemovalJobs()
	require.NoError(t, err)
	require.Equal(t, 1, pending, "removal must have enqueued a cascade job")

	require.Zero(t, i2Count(t, p),
		"a seat with a pending cascade job is the designed intermediate state, not a violation")
}

// TestI2ScanExemptsSystemBots covers exemption 4. System bots are admitted to
// project groups without a seat by design — the same exemption the admission
// gate applies — so the scan must use the SAME list, not a second copy.
func TestI2ScanExemptsSystemBots(t *testing.T) {
	srv, p := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	created := createProjectVia(t, srv, spaceA, token, "i2-bots")

	bots := systemBotUIDsForScan()
	require.NotEmpty(t, bots)

	groupNo := util.GenerUUID()
	seedProjectGroup(t, groupNo, spaceA, created.ProjectID)
	for _, bot := range bots {
		seedGroupMemberRow(t, groupNo, bot)
	}
	require.Zero(t, i2Count(t, p), "whitelisted system bots must be exempt")
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

// TestRemovingStallIsADistinctSignalFromI2 pins that the two alerts do not
// overlap: a stalled removal is NOT an I2 violation, because the member is still
// a member of record and the subset relation holds. Conflating them would make
// an operator treat a stuck worker as a security incident.
func TestRemovingStallIsADistinctSignalFromI2(t *testing.T) {
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

	// And it is NOT an I2 violation: the member still holds a seat of record.
	require.Zero(t, i2Count(t, p), "a stalled removal is not an invariant violation")
}
