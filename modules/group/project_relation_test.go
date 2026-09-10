package group

import (
	"context"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestProjectGroupLikeEscapesLiteralPatternCharacters(t *testing.T) {
	if got, want := projectGroupLike("100%!_done"), "%100!%!!!_done%"; got != want {
		t.Fatalf("projectGroupLike escaped pattern = %q, want %q", got, want)
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
