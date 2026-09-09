package group

// Two guards over the cascade, both for things a behaviour test cannot reach.
//
// PR #846's review measured that disabling the zero-row-promotion check kept
// every shipped test green. That is not a gap in the tests so much as a property
// of the fix: the successor is now picked under FOR UPDATE, so the promotion
// cannot match zero rows any more, and the branch that would catch it is
// unreachable by construction. What can still regress is the SHAPE — someone
// switching the promotion back to the primitive that swallows a zero-row write —
// and that is what these pin.

import (
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestTheCascadePromotesOnlyThroughTheCheckedPrimitive.
//
// UpdateMemberRoleTx's WHERE carries is_deleted = 0 and it reports nothing, so a
// promotion aimed at a row that is gone affects zero rows and returns nil. In
// the cascade that is not a lost update, it is a group with no creator at all:
// the demotion of the outgoing owner lands regardless, and the handover logs
// success. Promotions to creator therefore go through updateMemberRoleIfLiveTx,
// which reports rows affected.
//
// The DEMOTION is deliberately left on the unchecked primitive: its row is held
// under FOR UPDATE by queryGroupCreatorTx for the life of the transaction, and a
// no-op there costs nothing.
func TestTheCascadePromotesOnlyThroughTheCheckedPrimitive(t *testing.T) {
	src := readCascadeSource(t, "project_cascade.go")

	found := false
	for i, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "MemberRoleCreator") {
			continue
		}
		found = true
		require.NotContains(t, line, "UpdateMemberRoleTx(",
			"project_cascade.go:%d promotes to creator through the unchecked primitive. "+
				"A promotion that matches no live row must fail the step rather than fall "+
				"through to the demote — use updateMemberRoleIfLiveTx: %s", i+1, strings.TrimSpace(line))
	}
	require.True(t, found,
		"no promotion to MemberRoleCreator found in project_cascade.go — the guard stopped "+
			"matching and would pass vacuously")
	require.Contains(t, src, "updateMemberRoleIfLiveTx(",
		"the cascade must still promote through the checked primitive")
}

func readCascadeSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	require.NoError(t, err)
	var out strings.Builder
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		var stripped string
		stripped, inBlock = stripGoComments(line, inBlock)
		out.WriteString(stripped)
		out.WriteString("\n")
	}
	return out.String()
}

// TestEveryAdmissionEntryLabelIsEmitted is the acceptance line admission.go
// states and nothing checked: "every entry point's label is emitted at least
// once by the test suite — a label never emitted is a path that is not
// enforcing".
//
// The I2 matrix does drive all ten labels, so they ARE emitted; what was missing
// is anything that would notice if one stopped. This drives them itself rather
// than reading whatever an earlier case happened to leave behind, because the
// suite runs with -shuffle=on and a metric assertion that depends on test order
// is worse than none.
func TestEveryAdmissionEntryLabelIsEmitted(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	projectID := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "metric_outsider")
	seedProject(t, ctx, projectID, spaceID)
	// Deliberately no octo_project_member row: every entry must refuse.

	entries := []string{
		AdmissionEntryInviteConfirm,
		AdmissionEntryAddMembers,
		AdmissionEntryCreateGroup,
		AdmissionEntryCreateGroupBot,
		AdmissionEntryScanJoin,
		AdmissionEntryRegisterUser,
		AdmissionEntryOrgCreate,
		AdmissionEntryOrgEmployeeUpdate,
		AdmissionEntryPresetGroups,
		AdmissionEntryUnblacklist,
		// P2 — the all-member group admitter. Added to BOTH hard-coded lists, not
		// just this one: a new entry left out of them is an admission path outside
		// the I2 guarantee these two exist to assert, and the lists cannot see it
		// on their own.
		AdmissionEntryAllMemberGroup,
		AdmissionEntryAITeamGroup,
	}

	for _, entry := range entries {
		before := promtestutil.ToFloat64(
			admissionRejectedTotal.WithLabelValues(entry, admissionReasonNotProjectMember))

		groupNo := util.GenerUUID()
		seedGroupRow(t, ctx, groupNo, spaceID, projectID)
		version, err := ctx.GenSeq(common.GroupMemberSeqKey)
		require.NoError(t, err)
		tx, err := ctx.DB().Begin()
		require.NoError(t, err)
		err = f.db.admitOrRestoreMembersTx(tx, groupNo, spaceID, projectID,
			[]MemberAdmission{{UID: "metric_outsider", Version: version, Role: MemberRoleCommon}},
			entry)
		tx.RollbackUnlessCommitted()
		require.ErrorIs(t, err, ErrAdmissionRefused, "entry %s must refuse", entry)

		after := promtestutil.ToFloat64(
			admissionRejectedTotal.WithLabelValues(entry, admissionReasonNotProjectMember))
		require.Equal(t, before+1, after,
			"entry %s refused without moving its own label: the per-entry breakdown IS the "+
				"metric, and a path that refuses under someone else's label is invisible", entry)
	}
}
