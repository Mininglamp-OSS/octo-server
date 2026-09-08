package project

import (
	"testing"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestP2StatementsSurviveCollationDrift is the CI evidence for the COLLATE
// placement in every P2 statement that crosses the two schemas.
//
// # Why this cannot be left to reasoning
//
// octo_project* pin utf8mb4_general_ci; `group`, `group_member`, `user` and
// `robot` are 2019 legacy tables which the production mysqldump import left at
// utf8mb4_0900_ai_ci. An implicit comparison between them raises MySQL 1267 in
// production and passes in CI, whose database is created general_ci. A scan that
// 500s in production and is green in CI is worse than no scan: its gauge never
// publishes and the dashboard reads "no violations".
//
// The P2 scans run OUTSIDE OCTO_PROJECT_RECONCILE_ENABLED, on the argument that
// they survive the drift. That argument is only worth making if it is checked,
// and reconcile_p2.go's own header claimed these statements were covered here
// before they actually were — which is the shape of claim this file exists to
// stop being free.
//
// Both directions are asserted, drifted and converged, because a COLLATE written
// to work only against the drift breaks the day the conversion lands.
func TestP2StatementsSurviveCollationDrift(t *testing.T) {
	setup(t) // the shared schema is what CREATE TABLE ... LIKE copies from
	p, converge := newP1CollationProbe(t)

	const (
		probeProject = "p2_collation_project"
		probeGroup   = "p2_collation_group"
	)

	statements := map[string]func() error{
		// I4 scan A — active projects with no usable all-member group.
		"queryMissingAllMemberGroupPage": func() error {
			_, err := p.queryMissingAllMemberGroupPage(0, 10)
			return err
		},
		// I4 scan B — active project members missing from the all-member group.
		"queryAllMemberGroupGapPage": func() error {
			_, err := p.queryAllMemberGroupGapPage(0, "", 10)
			return err
		},
		// The read the admitter, the rename and the owner sync all go through.
		"queryAllMemberGroupNo": func() error {
			_, err := p.db.queryAllMemberGroupNo(probeProject)
			return err
		},
		// The write that unsticks a project whose group was detached.
		"clearStaleAllMemberGroupPointer": func() error {
			_, err := p.db.clearStaleAllMemberGroupPointer(probeProject)
			return err
		},
		// listMembers is deliberately NOT here, and that is a finding this test
		// produced rather than an omission. Its `user` join is PRE-EXISTING code
		// that carries no COLLATE, so it raises 1267 under drift — and
		// TestCollationDriftBreaksTheCrossSpaceQueriesAndTheConversionFixesThem
		// asserts exactly that, as the standing evidence for why the database-wide
		// conversion is needed. Asserting the opposite here would have pinned two
		// contradictory contracts on one statement.
		//
		// P2 added a `robot` join to it, WITH a COLLATE, so this change neither
		// creates nor widens that gap; the roster endpoint stays a known casualty
		// of the drift until the conversion lands.

		// D16's seat split. It used to join `user` with a COLLATE on the driving
		// side, which executes under drift (this test's subject) while planning as
		// a full scan of `user` (which this test cannot see — see the plan guard).
		// PR #855s seventh review; it is now two single-table reads and crosses no
		// schema at all, so it survives here for the stronger reason.
		"countActiveSeatsByKind": func() error {
			_, _, err := p.db.countActiveSeatsByKind(probeProject)
			return err
		},
		// D7's predicate, in pkg/project rather than this module — it runs on four
		// user-facing group endpoints, so a 1267 there is a user-visible 500 on
		// every exit, disband, removal and transfer in a project group.
		"pkg/project.IsAllMemberGroup": func() error {
			_, err := projectpkg.IsAllMemberGroup(p.db.session, probeProject, probeGroup)
			return err
		},
		// D6's successor pick, on the same path.
		"pkg/project.PickActiveOwner": func() error {
			_, err := projectpkg.PickActiveOwner(p.db.session, probeProject)
			return err
		},
	}

	for name, run := range statements {
		assert.NoError(t, run(),
			"%s must work on a collation-drifted database — that is why its "+
				"legacy-to-octo_project comparisons carry an explicit COLLATE, and why the "+
				"P2 scans run outside OCTO_PROJECT_RECONCILE_ENABLED. If this fails, add the "+
				"missing COLLATE or move the scan inside the gate; leaving it ungated and "+
				"failing produces a gauge that never publishes and reads as 'no violations'.",
			name)
	}

	converge()
	for name, run := range statements {
		assert.NoError(t, run(),
			"%s must STILL work once the legacy tables are converted: a COLLATE that only "+
				"works against the drift breaks at the conversion", name)
	}
	require.True(t, true)
}
