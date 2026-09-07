package group

// C1 and C2, asserted rather than claimed.
//
// PR #844's review found two acceptance items the code CLAIMS are covered and
// nothing checks:
//
//   - C1 — "Admission into a Space-direct group issues the same number of
//     database round-trips as before the change." admission.go says in as many
//     words that "the short-circuit is asserted by a test that COUNTS queries,
//     because 'I read the code and it returns early' is not evidence". There was
//     no such test.
//   - C2 — the spec calls the written column set "the largest regression surface
//     in P1" and asks for the full set to be asserted on BOTH the insert and the
//     restore branch. There were no column-level assertions anywhere.
//
// C1 is answered here with something stronger than a count: the gate is handed a
// transaction that has already been committed. Any statement issued on it fails,
// so "returns nil" means "issued nothing" — a proof rather than a number, and one
// that cannot drift as the surrounding code changes.

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spentTx returns a transaction that has been committed, so every statement on
// it fails. Using one as a probe turns "did this function query?" into a
// yes/no the test can read.
func spentTx(t *testing.T, ctx *config.Context) *dbr.Tx {
	t.Helper()
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return tx
}

// TestTheGateIssuesNoQueryForASpaceDirectGroup is C1.
//
// The gate is on the admission path of every group in the product. A gate that
// runs and passes is still a latency regression on every group join, so the
// requirement is not "cheap", it is "no query at all".
func TestTheGateIssuesNoQueryForASpaceDirectGroup(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	uids := []string{"c1_a", "c1_b", "c1_c"}

	// Space-direct: project_id is the empty sentinel.
	require.NoError(t,
		f.db.assertAdmissibleTx(spentTx(t, ctx), "sp_c1", "", uids, AdmissionEntryAddMembers),
		"the gate must return before issuing any statement for a Space-direct group; "+
			"this transaction is spent, so any query at all would have failed here")

	// An empty uid list is the other short-circuit.
	require.NoError(t,
		f.db.assertAdmissibleTx(spentTx(t, ctx), "sp_c1", util.GenerUUID(), nil, AdmissionEntryAddMembers))

	// A batch of nothing but exempt uids reduces to the empty case AFTER the
	// filter, which is the branch that would regress if the filter moved.
	require.NoError(t,
		f.db.assertAdmissibleTx(spentTx(t, ctx), "sp_c1", util.GenerUUID(),
			[]string{"", "u_10000", "fileHelper"}, AdmissionEntryAddMembers))

	// The control, and it has to be precise: a project group DOES query, and the
	// spent transaction must be what makes it fail. Without this, every assertion
	// above would also pass on a probe that was silently usable.
	//
	// The uids need real Space seats first. The Space half runs before the
	// project half and would refuse them outright, and a refusal would satisfy a
	// bare require.Error while proving nothing about whether the transaction was
	// ever touched.
	spaceID := "sp_" + util.GenerUUID()[:8]
	for _, uid := range uids {
		seedSpaceSeat(t, ctx, spaceID, uid)
	}
	err := f.db.assertAdmissibleTx(spentTx(t, ctx), spaceID, util.GenerUUID(), uids,
		AdmissionEntryAddMembers)
	require.Error(t, err, "a project group must reach the in-transaction project check")
	require.NotErrorIs(t, err, ErrAdmissionRefused,
		"the control must fail on the spent transaction, not on a membership refusal — "+
			"otherwise it does not prove the probe works: %v", err)
	require.Contains(t, err.Error(), "admission project check",
		"the failure must come from the in-transaction half: %v", err)
}

// memberRow is the full written column set C2 is about.
type memberRow struct {
	Remark             string `db:"remark"`
	Role               int    `db:"role"`
	BotAdmin           int    `db:"bot_admin"`
	Version            int64  `db:"version"`
	Status             int    `db:"status"`
	Vercode            string `db:"vercode"`
	IsDeleted          int    `db:"is_deleted"`
	InviteUID          string `db:"invite_uid"`
	Robot              int    `db:"robot"`
	ForbiddenExpirTime int64  `db:"forbidden_expir_time"`
	IsExternal         int    `db:"is_external"`
	SourceSpaceID      string `db:"source_space_id"`
}

func readMemberRow(t *testing.T, ctx *config.Context, groupNo, uid string) memberRow {
	t.Helper()
	var rows []memberRow
	_, err := ctx.DB().SelectBySql(
		"SELECT remark, role, bot_admin, `version`, status, vercode, is_deleted, invite_uid, "+
			"robot, forbidden_expir_time, is_external, source_space_id "+
			"FROM group_member WHERE group_no = ? AND uid = ?", groupNo, uid).Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

func admitFull(t *testing.T, f *Group, groupNo, spaceID, projectID string, a MemberAdmission, entry string) error {
	t.Helper()
	tx, err := f.ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	if err := f.db.admitOrRestoreMembersTx(tx, groupNo, spaceID, projectID,
		[]MemberAdmission{a}, entry); err != nil {
		return err
	}
	return tx.Commit()
}

// TestTheInsertBranchWritesTheWholeColumnSet is C2's first half.
func TestTheInsertBranchWritesTheWholeColumnSet(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "c2_new")
	seedGroupRow(t, ctx, groupNo, spaceID, "")

	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	require.NoError(t, admitFull(t, f, groupNo, spaceID, "", MemberAdmission{
		UID:           "c2_new",
		Version:       version,
		Role:          MemberRoleCommon,
		InviteUID:     "op1",
		Robot:         1,
		Vercode:       "vc-fixed",
		IsExternal:    1,
		SourceSpaceID: "sp_origin",
	}, AdmissionEntryAddMembers))

	got := readMemberRow(t, ctx, groupNo, "c2_new")
	assert.Equal(t, memberRow{
		Remark:             "",
		Role:               MemberRoleCommon,
		BotAdmin:           0,
		Version:            version,
		Status:             int(common.GroupMemberStatusNormal),
		Vercode:            "vc-fixed",
		IsDeleted:          0,
		InviteUID:          "op1",
		Robot:              1,
		ForbiddenExpirTime: 0,
		IsExternal:         1,
		SourceSpaceID:      "sp_origin",
	}, got)
}

// TestTheRestoreBranchReproducesRecoverMemberTx is C2's second half, and the one
// the spec singles out.
//
// The contract the funnel inherited from recoverMemberTx, stated as assertions:
// remark / role / version / invite_uid / is_external / source_space_id are
// REPLACED, bot_admin is RESET (a group-granted permission must not survive
// leave-and-rejoin), and vercode / status / robot / forbidden_expir_time are
// deliberately left alone.
func TestTheRestoreBranchReproducesRecoverMemberTx(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "c2_back")
	seedGroupRow(t, ctx, groupNo, spaceID, "")

	// A departed member carrying a distinctive value in every column, so an
	// assignment that goes missing cannot be masked by a default.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, bot_admin, `version`, status, "+
			"vercode, is_deleted, invite_uid, robot, forbidden_expir_time, is_external, "+
			"source_space_id, created_at) "+
			"VALUES (?, ?, 'old-remark', ?, 1, 7, ?, 'vc-original', 1, 'old-op', 1, 999, 1, "+
			"'sp_old', NOW())",
		groupNo, "c2_back", MemberRoleManager, int(common.GroupMemberStatusBlacklist),
	).Exec()
	require.NoError(t, err)

	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	require.NoError(t, admitFull(t, f, groupNo, spaceID, "", MemberAdmission{
		UID:           "c2_back",
		Version:       version,
		Role:          MemberRoleCommon,
		InviteUID:     "new-op",
		Robot:         0,
		Vercode:       "vc-ignored",
		IsExternal:    0,
		SourceSpaceID: "",
	}, AdmissionEntryAddMembers))

	got := readMemberRow(t, ctx, groupNo, "c2_back")
	assert.Equal(t, memberRow{
		// Replaced.
		Remark:        "",
		Role:          MemberRoleCommon,
		Version:       version,
		InviteUID:     "new-op",
		IsExternal:    0,
		SourceSpaceID: "",
		IsDeleted:     0,
		// Reset: a bot-admin grant must not survive leave-and-rejoin.
		BotAdmin: 0,
		// Preserved, deliberately — recoverMemberTx did not touch these.
		Vercode:            "vc-original",
		Status:             int(common.GroupMemberStatusBlacklist),
		Robot:              1,
		ForbiddenExpirTime: 999,
	}, got)
}

// TestReAddingAnActiveMemberChangesNothing is the third branch of the same
// statement, and the one with a user-visible failure mode: an unconditional
// upsert would DEMOTE a group admin who happens to be re-added, and "re-add" is
// reachable from the preset-group path on every Space join.
func TestReAddingAnActiveMemberChangesNothing(t *testing.T) {
	_, ctx := newTestServer(t)
	f := New(ctx)

	spaceID := "sp_" + util.GenerUUID()[:8]
	groupNo := util.GenerUUID()
	seedSpaceSeat(t, ctx, spaceID, "c2_active")
	seedGroupRow(t, ctx, groupNo, spaceID, "")

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, remark, role, bot_admin, `version`, status, "+
			"vercode, is_deleted, invite_uid, robot, forbidden_expir_time, is_external, "+
			"source_space_id, created_at) "+
			"VALUES (?, ?, 'keep', ?, 1, 7, ?, 'vc-keep', 0, 'first-op', 0, 5, 0, '', NOW())",
		groupNo, "c2_active", MemberRoleManager, int(common.GroupMemberStatusNormal),
	).Exec()
	require.NoError(t, err)
	before := readMemberRow(t, ctx, groupNo, "c2_active")

	version, err := ctx.GenSeq(common.GroupMemberSeqKey)
	require.NoError(t, err)
	require.NoError(t, admitFull(t, f, groupNo, spaceID, "", MemberAdmission{
		UID:       "c2_active",
		Version:   version,
		Role:      MemberRoleCommon,
		InviteUID: "second-op",
	}, AdmissionEntryPresetGroups))

	assert.Equal(t, before, readMemberRow(t, ctx, groupNo, "c2_active"),
		"re-adding an already active member must change nothing — not the role, not the "+
			"version, not the remark")
}
