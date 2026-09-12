package group

import (
	"context"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestChangedProjectSourceAllowedFailsClosedForUnobservedRebind(t *testing.T) {
	tests := []struct {
		name                         string
		initialSource, currentSource string
		target                       string
		want                         bool
	}{
		{name: "unchanged", initialSource: "p-old", currentSource: "p-old", target: "p-new", want: true},
		{name: "concurrent unbind", initialSource: "p-old", currentSource: "", target: "p-new", want: true},
		{name: "same target rebind", initialSource: "p-old", currentSource: "p-new", target: "p-new", want: true},
		{name: "unobserved replacement", initialSource: "p-old", currentSource: "p-other", target: "p-new", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changedProjectSourceAllowed(tt.initialSource, tt.currentSource, tt.target); got != tt.want {
				t.Fatalf("changedProjectSourceAllowed(%q, %q, %q) = %v, want %v", tt.initialSource, tt.currentSource, tt.target, got, tt.want)
			}
		})
	}
}

func TestGroupProjectRelationProjectionPreservesLegacyUnknownActorAsNull(t *testing.T) {
	legacy := groupProjectRelationFromRow(&groupProjectRelationRow{
		GroupNo: "g-legacy", Name: "Legacy", ProjectID: "p1",
	})
	if legacy.ProjectID != "p1" {
		t.Fatalf("legacy Project relation id = %q, want p1", legacy.ProjectID)
	}
	if legacy.LinkedBy != nil {
		t.Fatalf("legacy missing linked_by = %#v, want nil", legacy.LinkedBy)
	}

	unbound := groupProjectRelationFromRow(&groupProjectRelationRow{
		GroupNo: "g-unbound", Name: "Unbound",
	})
	if unbound.ProjectID != "" || unbound.LinkedBy != nil {
		t.Fatalf("unbound relation = %#v, want empty project and nil actor", unbound)
	}

	actor := "owner"
	current := groupProjectRelationFromRow(&groupProjectRelationRow{
		GroupNo: "g-current", Name: "Current", ProjectID: "p1", ProjectLinkedBy: &actor,
	})
	if current.LinkedBy == nil || *current.LinkedBy != actor {
		t.Fatalf("current linked_by = %#v, want %q", current.LinkedBy, actor)
	}
}

func TestValidateGroupProjectRelationRejectsStaleActorOnUnboundGroup(t *testing.T) {
	actor := "operator"
	err := validateGroupProjectRelation(&groupProjectRelationRow{
		GroupNo: "g", ProjectLinkedBy: &actor,
	})
	if err == nil {
		t.Fatal("unbound group retaining a non-empty relation actor must be rejected")
	}
}
func TestProjectRelationMutationRejectsAIContainerAndBumpsGroupVersion(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)

	spaceID := "space-relation-version-" + util.GenerUUID()[:8]
	projectID := "project-relation-version-" + util.GenerUUID()[:8]
	actorUID := "relation-version-owner-" + util.GenerUUID()[:8]
	groupNo := "group-relation-version-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, actorUID)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, actorUID, 0)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, status, is_destroy, robot) VALUES (?, ?, 1, 0, 0)",
		actorUID, "Relation version owner",
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Relation version group", Creator: actorUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: actorUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	var initial struct {
		Version int64 `db:"version"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT version FROM `group` WHERE group_no=?", groupNo,
	).LoadOne(&initial))

	bound, err := g.bindGroupProject(actorUID, groupNo, projectID)
	require.NoError(t, err)
	assert.Equal(t, projectID, bound.ProjectID)
	var afterBind struct {
		ProjectID string `db:"project_id"`
		Version   int64  `db:"version"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id, version FROM `group` WHERE group_no=?", groupNo,
	).LoadOne(&afterBind))
	assert.Equal(t, projectID, afterBind.ProjectID)
	assert.NotEqual(t, initial.Version, afterBind.Version,
		"binding a Project must advance the group version for incremental clients")

	repeated, err := g.bindGroupProject(actorUID, groupNo, projectID)
	require.NoError(t, err)
	assert.Equal(t, projectID, repeated.ProjectID)
	var afterRepeat struct {
		ProjectID string `db:"project_id"`
		Version   int64  `db:"version"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id, version FROM `group` WHERE group_no=?", groupNo,
	).LoadOne(&afterRepeat))
	assert.Equal(t, afterBind.Version, afterRepeat.Version,
		"repeated binding must not advance the group version")

	unbound, err := g.unbindGroupProject(actorUID, groupNo)
	require.NoError(t, err)
	assert.Empty(t, unbound.ProjectID)
	var afterUnbind struct {
		ProjectID string `db:"project_id"`
		Version   int64  `db:"version"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id, version FROM `group` WHERE group_no=?", groupNo,
	).LoadOne(&afterUnbind))
	assert.Empty(t, afterUnbind.ProjectID)
	assert.NotEqual(t, afterBind.Version, afterUnbind.Version,
		"unbinding a Project must advance the group version for incremental clients")

	aiUnboundNo := "group-ai-relation-bind-" + util.GenerUUID()[:8]
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: aiUnboundNo, Name: "AI relation bind", Creator: actorUID,
		Purpose: aiteampkg.GroupPurpose, Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: aiUnboundNo, UID: actorUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	_, err = g.bindGroupProject(actorUID, aiUnboundNo, projectID)
	require.ErrorIs(t, err, aiteampkg.ErrContainerProtected)
	assertGroupProjectRelationRow(t, ctx, aiUnboundNo, "", "")

	aiBoundNo := "group-ai-relation-unbind-" + util.GenerUUID()[:8]
	linkedBy := actorUID
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: aiBoundNo, Name: "AI relation unbind", Creator: actorUID,
		Purpose: aiteampkg.GroupPurpose, Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
		ProjectID: projectID, ProjectLinkedBy: &linkedBy,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: aiBoundNo, UID: actorUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	_, err = g.unbindGroupProject(actorUID, aiBoundNo)
	require.ErrorIs(t, err, aiteampkg.ErrContainerProtected)
	assertGroupProjectRelationRow(t, ctx, aiBoundNo, projectID, linkedBy)
}
func TestProjectDisbandDetachesRelationPreservingNativeMembers(t *testing.T) {
	_, ctx := newTestServer(t)
	g := New(ctx)
	projectID := "project-" + util.GenerUUID()
	spaceID := "space-" + util.GenerUUID()
	groupNo := "group-" + util.GenerUUID()
	linkedBy := testutil.UID
	nativeUID := "native-" + util.GenerUUID()

	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Project group", Creator: linkedBy,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
		ProjectID: projectID, ProjectLinkedBy: &linkedBy,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: nativeUID, Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	require.NoError(t, g.revertProjectGroupsToSpace(ctx, projectmod.ProjectDisband{
		ProjectID: projectID, SpaceID: spaceID,
	}))

	var relation struct {
		ProjectID string `db:"project_id"`
		LinkedBy  string `db:"linked_by"`
	}
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT project_id, COALESCE(project_linked_by, '') AS linked_by FROM `group` WHERE group_no = ?", groupNo,
	).LoadOne(&relation))
	assert.Empty(t, relation.ProjectID, "disband must clear the Project relation")
	assert.Empty(t, relation.LinkedBy, "disband must clear the relation actor with project_id")

	var nativeCount int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM group_member WHERE group_no = ? AND uid = ? AND is_deleted = 0",
		groupNo, nativeUID,
	).LoadOne(&nativeCount))
	assert.Equal(t, 1, nativeCount, "Project disband must preserve native group membership")
}

func TestProjectRelationReadDoesNotWaitOnUncommittedSpaceSeatWrite(t *testing.T) {
	_, ctx := newTestServer(t)
	g := New(ctx)
	projectID := "project-" + util.GenerUUID()
	spaceID := "space-" + util.GenerUUID()
	groupNo := "group-" + util.GenerUUID()
	actorUID := "relation-reader-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, actorUID)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, actorUID, 0)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, status, is_destroy, robot) VALUES (?, ?, 1, 0, 0)",
		actorUID, "Relation reader",
	).Exec()
	require.NoError(t, err)
	linkedBy := actorUID
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Project group", Creator: actorUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
		ProjectID: projectID, ProjectLinkedBy: &linkedBy,
	}))

	writer, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer writer.RollbackUnlessCommitted()
	_, err = writer.Update("space_member").
		Set("status", 0).
		Where("space_id=? AND uid=?", spaceID, actorUID).
		Exec()
	require.NoError(t, err)

	type result struct {
		relation GroupProjectRelation
		err      error
	}
	done := make(chan result, 1)
	go func() {
		relation, readErr := g.readGroupProject(context.Background(), groupNo, actorUID)
		done <- result{relation: relation, err: readErr}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, projectID, got.relation.ProjectID)
	case <-time.After(2 * time.Second):
		_ = writer.Rollback()
		t.Fatal("repeatable-read relation authorization waited on an uncommitted Space-seat writer")
	}
}
func TestProjectRelationRejectsDisabledActorAcrossBoundAndNativePaths(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)
	projectID := "project-" + util.GenerUUID()
	spaceID := "space-" + util.GenerUUID()
	groupNo := "group-" + util.GenerUUID()
	actorUID := "relation-disabled-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, actorUID)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, actorUID, 0)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, status, is_destroy, robot) VALUES (?, ?, 1, 0, 0)",
		actorUID, "Disabled relation actor",
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Native group", Creator: actorUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: actorUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	_, err = ctx.DB().Update("user").Set("status", 0).Where("uid=?", actorUID).Exec()
	require.NoError(t, err)

	_, err = g.bindGroupProject(actorUID, groupNo, projectID)
	require.ErrorIs(t, err, errProjectRelationForbidden,
		"a disabled actor cannot bind a Project even with an active Project seat")
	_, err = g.readGroupProject(context.Background(), groupNo, actorUID)
	require.ErrorIs(t, err, errProjectRelationForbidden,
		"a disabled actor cannot read the native relation path")
	_, err = g.unbindGroupProject(actorUID, groupNo)
	require.ErrorIs(t, err, errProjectRelationForbidden,
		"a disabled actor cannot unbind a native relation")
}

func TestProjectMemberWithoutNativeManagerCannotBindOrUnbind(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)

	spaceID := "space-relation-manager-" + util.GenerUUID()[:8]
	projectID := "project-relation-manager-" + util.GenerUUID()[:8]
	actorUID := "relation-project-member-" + util.GenerUUID()[:8]
	ownerUID := "relation-native-owner-" + util.GenerUUID()[:8]
	bindGroupNo := "group-relation-bind-" + util.GenerUUID()[:8]
	unbindGroupNo := "group-relation-unbind-" + util.GenerUUID()[:8]

	seedSpaceSeat(t, ctx, spaceID, actorUID)
	seedProject(t, ctx, projectID, spaceID)
	seedProjectMember(t, ctx, projectID, spaceID, actorUID, 0)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, status, is_destroy, robot) VALUES (?, ?, 1, 0, 0)",
		actorUID, "Project member without native manager",
	).Exec()
	require.NoError(t, err)

	require.NoError(t, g.db.Insert(&Model{
		GroupNo: bindGroupNo, Name: "Unbound relation", Creator: ownerUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: bindGroupNo, UID: ownerUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: bindGroupNo, UID: actorUID, Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	beforeBindMembers := relationMemberSnapshot(t, ctx, bindGroupNo)
	_, err = g.bindGroupProject(actorUID, bindGroupNo, projectID)
	require.ErrorIs(t, err, errProjectRelationForbidden)
	assert.Equal(t, beforeBindMembers, relationMemberSnapshot(t, ctx, bindGroupNo),
		"failed bind must not change native group membership")
	assertGroupProjectRelationRow(t, ctx, bindGroupNo, "", "")

	linkedBy := ownerUID
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: unbindGroupNo, Name: "Bound relation", Creator: ownerUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
		ProjectID: projectID, ProjectLinkedBy: &linkedBy,
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: unbindGroupNo, UID: ownerUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: unbindGroupNo, UID: actorUID, Role: MemberRoleCommon,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	beforeUnbindMembers := relationMemberSnapshot(t, ctx, unbindGroupNo)
	_, err = g.unbindGroupProject(actorUID, unbindGroupNo)
	require.ErrorIs(t, err, errProjectRelationForbidden)
	assert.Equal(t, beforeUnbindMembers, relationMemberSnapshot(t, ctx, unbindGroupNo),
		"failed unbind must not change native group membership")
	assertGroupProjectRelationRow(t, ctx, unbindGroupNo, projectID, linkedBy)
}

type relationMemberSnapshotRow struct {
	UID       string `db:"uid"`
	Role      int    `db:"role"`
	Status    int    `db:"status"`
	IsDeleted int    `db:"is_deleted"`
}

func relationMemberSnapshot(t *testing.T, ctx *config.Context, groupNo string) []relationMemberSnapshotRow {
	t.Helper()
	var rows []relationMemberSnapshotRow
	_, err := ctx.DB().SelectBySql(
		"SELECT uid, role, status, is_deleted FROM group_member WHERE group_no=? ORDER BY uid",
		groupNo,
	).Load(&rows)
	require.NoError(t, err)
	return rows
}

func assertGroupProjectRelationRow(t *testing.T, ctx *config.Context, groupNo, wantProjectID, wantLinkedBy string) {
	t.Helper()
	var row struct {
		ProjectID  string  `db:"project_id"`
		ProjectUID *string `db:"project_linked_by"`
	}
	err := ctx.DB().SelectBySql(
		"SELECT project_id, project_linked_by FROM `group` WHERE group_no=?",
		groupNo,
	).LoadOne(&row)
	require.NoError(t, err)
	assert.Equal(t, wantProjectID, row.ProjectID)
	if wantLinkedBy == "" {
		assert.True(t, row.ProjectUID == nil || *row.ProjectUID == "")
		return
	}
	require.NotNil(t, row.ProjectUID)
	assert.Equal(t, wantLinkedBy, *row.ProjectUID)
}

func TestProjectRelationReadRejectsCrossSpaceProjectMember(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)
	projectID := "project-cross-space-" + util.GenerUUID()[:8]
	projectSpaceID := "space-project-" + util.GenerUUID()[:8]
	memberSpaceID := "space-member-" + util.GenerUUID()[:8]
	groupNo := "group-cross-space-" + util.GenerUUID()[:8]
	actorUID := "cross-space-actor-" + util.GenerUUID()[:8]

	seedSpaceSeat(t, ctx, projectSpaceID, actorUID)
	seedSpaceSeat(t, ctx, memberSpaceID, actorUID)
	seedProject(t, ctx, projectID, projectSpaceID)
	seedProjectMember(t, ctx, projectID, memberSpaceID, actorUID, 0)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, status, is_destroy, robot) VALUES (?, ?, 1, 0, 0)",
		actorUID, "Cross-space actor",
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Cross-space relation", Creator: actorUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: projectSpaceID,
		ProjectID: projectID,
	}))

	_, err = g.readGroupProject(context.Background(), groupNo, actorUID)
	require.ErrorIs(t, err, errProjectRelationNotFound,
		"Project membership from another Space must not authorize relation reads")
}

// TestProjectRemovalRejoinRepairsMissingNativeMember pins the stale-removal
// boundary: the Project seat can already be active again while the old
// callback observes no native row. It must reconcile the current dedicated
// projection (including IMAdd) rather than returning and letting a later
// IMRemove strand the active member.
func TestProjectRemovalRejoinRepairsMissingNativeMember(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)
	stub := newGroupIMStub(t, ctx)

	spaceID := "space-project-rejoin-" + util.GenerUUID()[:8]
	projectID := "project-rejoin-" + util.GenerUUID()[:8]
	groupNo := "group-rejoin-" + util.GenerUUID()[:8]
	uid := "project-rejoin-user-" + util.GenerUUID()[:8]
	ownerUID := "project-rejoin-owner-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, ownerUID)
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, ownerUID)
	seedProjectSeat(t, ctx, projectID, spaceID, uid, 0)
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, ownerUID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
		ownerUID, ownerUID, "pro_"+util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
		uid, uid, "pro_"+util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: ownerUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	err = g.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
		ProjectID:   projectID,
		SpaceID:     spaceID,
		UID:         uid,
		OperatorUID: ownerUID,
		Reason:      "kicked",
	})
	require.NoError(t, err)
	assert.True(t, activeMemberExists(t, ctx, groupNo, uid),
		"active Project membership with a missing native row must be restored")
	assert.Contains(t, stub.subscribed(groupNo), uid,
		"the repair must issue IMAdd for the restored native membership")
	assert.NotContains(t, stub.unsubscribed(groupNo), uid,
		"the stale removal callback must not issue IMRemove after rejoin")
}

// TestProjectRejoinRestoresNativeCurrentOwnerForSpaceIDVariants drives the
// real Group admission and Owner-sync hooks. A rejoin selector that differs
// from the stored Space ID by case or PAD SPACE must restore the native row,
// then converge the current Project Owner to creator.
func TestProjectRejoinRestoresNativeCurrentOwnerForSpaceIDVariants(t *testing.T) {
	cases := []struct {
		name          string
		storedSpaceID func(string) string
	}{
		{
			name:          "case_variant",
			storedSpaceID: strings.ToUpper,
		},
		{
			name:          "pad_space",
			storedSpaceID: func(base string) string { return base + " " },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := newTestServer(t)
			defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
			g := New(ctx)
			stub := newGroupIMStub(t, ctx)

			baseSpaceID := "space-project-owner-" + util.GenerUUID()[:8]
			storedSpaceID := tc.storedSpaceID(baseSpaceID)
			projectID := "project-owner-rejoin-" + util.GenerUUID()[:8]
			groupNo := "group-owner-rejoin-" + util.GenerUUID()[:8]
			ownerUID := "project-owner-rejoin-" + util.GenerUUID()[:8]

			seedSpaceSeat(t, ctx, storedSpaceID, ownerUID)
			seedProjectForGroupTest(t, ctx, projectID, storedSpaceID, ownerUID)
			seedAllMemberGroupRow(t, ctx, groupNo, projectID, storedSpaceID, ownerUID)
			_, err := ctx.DB().InsertBySql(
				"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
				ownerUID, ownerUID, "owner_"+util.GenerUUID()[:8],
			).Exec()
			require.NoError(t, err)

			// No native owner row exists: this is the gap the rejoin projection
			// must repair. The real admitter starts a restored row as common;
			// the real owner hook must then promote the current Project Owner.
			require.NoError(t, g.admitToAllMemberGroup(
				ctx, baseSpaceID, groupNo, ownerUID,
			))
			role, present := liveMemberRole(t, ctx, groupNo, ownerUID)
			require.True(t, present, "rejoin must restore the native member")
			assert.Equal(t, MemberRoleCommon, role,
				"admission itself must not mint creator privileges")

			require.NoError(t, g.ensureAllMemberGroupOwner(ctx, projectID, groupNo))
			role, present = liveMemberRole(t, ctx, groupNo, ownerUID)
			require.True(t, present)
			assert.Equal(t, MemberRoleCreator, role,
				"the current human Project Owner must own the restored projection")
			assert.Contains(t, stub.subscribed(groupNo), ownerUID)
		})
	}
}

// TestProjectRemovalRejoinCannotLateUnsubscribe exercises the opposite race:
// the stale removal starts with no valid seats, blocks in IMRemove, then a
// rejoin commits and admits the member before the old broker call returns.
// The stale call must perform a final current-pointer reconciliation and leave
// the subscriber present.
func TestProjectRemovalRejoinCannotLateUnsubscribe(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	g := New(ctx)
	stub := newGroupIMStub(t, ctx)
	stub.blockSubscriberRemove = true

	spaceID := "space-project-race-" + util.GenerUUID()[:8]
	projectID := "project-race-" + util.GenerUUID()[:8]
	groupNo := "group-race-" + util.GenerUUID()[:8]
	ownerUID := "project-race-owner-" + util.GenerUUID()[:8]
	uid := "project-race-user-" + util.GenerUUID()[:8]
	seedSpaceSeat(t, ctx, spaceID, ownerUID)
	seedSpaceSeat(t, ctx, spaceID, uid)
	seedProjectForGroupTest(t, ctx, projectID, spaceID, ownerUID)
	seedProjectSeat(t, ctx, projectID, spaceID, uid, 0)
	seedAllMemberGroupRow(t, ctx, groupNo, projectID, spaceID, ownerUID)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
		ownerUID, ownerUID, "rac_"+util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
		uid, uid, "rac_"+util.GenerUUID()[:8],
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: ownerUID, Role: MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))
	_, err = ctx.DB().UpdateBySql(
		"UPDATE space_member SET status=0 WHERE space_id=? AND uid=?",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE octo_project_member SET status=0, removing=1 WHERE project_id=? AND uid=?",
		projectID, uid,
	).Exec()
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		errCh <- g.detachMemberFromProjectGroups(ctx, projectmod.MemberRemoval{
			ProjectID: projectID, SpaceID: spaceID, UID: uid,
			OperatorUID: ownerUID, Reason: "kicked",
		})
	}()
	select {
	case <-stub.removeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("stale removal did not reach the blocked IMRemove")
	}

	_, err = ctx.DB().UpdateBySql(
		"UPDATE space_member SET status=1 WHERE space_id=? AND uid=?",
		spaceID, uid,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE octo_project_member SET status=1, removing=0 WHERE project_id=? AND uid=?",
		projectID, uid,
	).Exec()
	require.NoError(t, err)
	require.NoError(t, g.admitToAllMemberGroup(ctx, spaceID, groupNo, uid))
	stub.failNextSubscriberAdd()
	close(stub.releaseRemove)
	require.ErrorIs(t, <-errCh, projectpkg.ErrAdmittedButNotSubscribed)

	var pendingRejoin []int
	_, err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member_removal_cleanup "+
			"WHERE space_id=? AND uid=? AND reason=? AND status=0",
		spaceID, uid, spacemod.MemberRemoveReasonRejoined,
	).Load(&pendingRejoin)
	require.NoError(t, err)
	require.Len(t, pendingRejoin, 1)
	assert.Equal(t, 1, pendingRejoin[0],
		"failed compensation after late IMRemove must leave a durable rejoin intent")
	require.False(t, stub.currentlySubscribed(groupNo, uid))
	require.NoError(t, g.reconcileDedicatedGroupProjection(ctx, projectmod.MemberRemoval{
		ProjectID: projectID, SpaceID: spaceID, UID: uid, OperatorUID: ownerUID,
	}, groupNo))
	assert.True(t, stub.currentlySubscribed(groupNo, uid),
		"retrying the projection must restore the subscriber")
}
