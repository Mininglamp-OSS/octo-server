package workspace_test

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	workspacemod "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceServiceOwnerAdminMemberBoundaries(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-admin", "Workspace admin"},
		{"ws-member", "Workspace member"},
		{"ws-target", "Workspace target"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "ws-boundary-space", "10000", "ws-admin", "ws-member", "ws-target")

	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-boundary-space", "Boundary")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{
		{UID: "ws-admin", WorkspaceRole: workspacemod.WorkspaceRoleAdmin},
		{UID: "ws-member", WorkspaceRole: workspacemod.WorkspaceRoleMember},
		{UID: "ws-target", WorkspaceRole: workspacemod.WorkspaceRoleMember},
	})
	require.NoError(t, err)

	newDescription := "member must not edit"
	_, err = svc.Update(workspacemod.Scope{UID: "ws-member"}, ws.WorkspaceID, workspacemod.UpdateRequest{Description: &newDescription})
	require.Error(t, err)
	require.True(t, errors.Is(err, workspacemod.ErrForbidden))
	_, err = svc.AddMembers(workspacemod.Scope{UID: "ws-member"}, ws.WorkspaceID, []workspacemod.MemberInput{{UID: "ws-target", WorkspaceRole: workspacemod.WorkspaceRoleMember}})
	require.Error(t, err)
	require.True(t, errors.Is(err, workspacemod.ErrForbidden))

	adminDescription := "admin can edit"
	_, err = svc.Update(workspacemod.Scope{UID: "ws-admin"}, ws.WorkspaceID, workspacemod.UpdateRequest{Description: &adminDescription})
	require.NoError(t, err)
	_, err = svc.UpdateMember(workspacemod.Scope{UID: "ws-admin"}, ws.WorkspaceID, "ws-target", workspacemod.WorkspaceRoleAdmin)
	require.NoError(t, err)
	// Admins can manage another Admin; this is intentionally broader than the
	// old group-manager policy and is part of the Workspace contract.

	require.NoError(t, svc.RemoveMember(workspacemod.Scope{UID: "ws-admin"}, ws.WorkspaceID, "ws-target"))
	selfDemoted, err := svc.UpdateMember(workspacemod.Scope{UID: "ws-admin"}, ws.WorkspaceID, "ws-admin", workspacemod.WorkspaceRoleMember)
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceRoleMember, selfDemoted.WorkspaceRole)

	require.ErrorIs(t, svc.RemoveMember(workspacemod.Scope{UID: "ws-member"}, ws.WorkspaceID, "ws-admin"), workspacemod.ErrForbidden)
	require.ErrorIs(t, svc.RemoveMember(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "10000"), workspacemod.ErrOwnerProtected)
	require.ErrorIs(t, svc.Leave(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID), workspacemod.ErrOwnerProtected)

	transferred, err := svc.TransferOwner(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-admin")
	require.NoError(t, err)
	require.Equal(t, "ws-admin", transferred.OwnerUID)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, transferred.WorkspaceRole)
	oldOwner, err := svc.Get(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID)
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, oldOwner.WorkspaceRole)
	newOwner, err := svc.Get(workspacemod.Scope{UID: "ws-admin"}, ws.WorkspaceID)
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceRoleOwner, newOwner.WorkspaceRole)
}

func TestWorkspaceServiceDisabledOwnerDoesNotSuspendWorkspace(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, account := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-disabled-owner-admin", "Workspace admin"},
		{"ws-disabled-owner-member", "Workspace member"},
	} {
		seedWorkspaceUser(t, ctx, account.uid, account.name)
	}
	seedWorkspaceSpace(t, ctx, "ws-disabled-owner-space", "10000", "ws-disabled-owner-admin", "ws-disabled-owner-member")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-disabled-owner-space", "Disabled owner")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{
		{UID: "ws-disabled-owner-admin", WorkspaceRole: workspacemod.WorkspaceRoleAdmin},
		{UID: "ws-disabled-owner-member", WorkspaceRole: workspacemod.WorkspaceRoleMember},
	})
	require.NoError(t, err)
	_, err = ctx.DB().Update("user").Set("status", 0).Where("uid=?", "10000").Exec()
	require.NoError(t, err)

	_, err = svc.Get(workspacemod.Scope{UID: "ws-disabled-owner-admin"}, ws.WorkspaceID)
	require.NoError(t, err, "an eligible admin can still read a Workspace after its owner is disabled")
	description := "edited while owner disabled"
	_, err = svc.Update(workspacemod.Scope{UID: "ws-disabled-owner-admin"}, ws.WorkspaceID,
		workspacemod.UpdateRequest{Description: &description})
	require.NoError(t, err, "an eligible admin can still manage a Workspace after its owner is disabled")
	_, err = svc.Get(workspacemod.Scope{UID: "ws-disabled-owner-member"}, ws.WorkspaceID)
	require.NoError(t, err, "an eligible member can still read a Workspace after its owner is disabled")
	require.NoError(t, svc.Leave(workspacemod.Scope{UID: "ws-disabled-owner-member"}, ws.WorkspaceID))

	_, err = svc.Get(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID)
	require.ErrorIs(t, err, workspacemod.ErrForbidden, "a disabled owner cannot act as a Workspace user")
	_, err = svc.List(workspacemod.Scope{UID: "10000"}, "ws-disabled-owner-space", "", workspacemod.Page{Index: 1, Size: 20})
	require.ErrorIs(t, err, workspacemod.ErrForbidden, "disabled users cannot list retained Workspace relations")
}

func TestWorkspaceServicePartialUpdateAndNoop(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-partial-space", "10000")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-partial-space", "Initial")

	require.ErrorIs(t, func() error {
		_, err := svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{})
		return err
	}(), workspacemod.ErrRequestInvalid)

	newName := "Renamed"
	updated, err := svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Name: &newName})
	require.NoError(t, err)
	require.Equal(t, "Renamed", updated.Name)
	require.Equal(t, ws.Description, updated.Description)
	require.Equal(t, ws.Logo, updated.Logo)

	description := "description"
	updated, err = svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Description: &description})
	require.NoError(t, err)
	require.Equal(t, "Renamed", updated.Name)
	require.Equal(t, description, updated.Description)
	longLogo := "https://example.invalid/" + strings.Repeat("x", 220)
	updated, err = svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Logo: &longLogo})
	require.NoError(t, err)
	require.Equal(t, longLogo, updated.Logo)

	// Seed a stable old timestamp so a same-value update cannot pass by merely
	// racing the clock. The operation must be a true no-op, including metadata.
	oldUpdatedAt := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err = ctx.DB().Update("octo_workspace").Set("updated_at", oldUpdatedAt).
		Where("workspace_id=?", ws.WorkspaceID).Exec()
	require.NoError(t, err)
	var beforeRow struct {
		UpdatedAt time.Time `db:"updated_at"`
	}
	require.NoError(t, ctx.DB().Select("updated_at").From("octo_workspace").
		Where("workspace_id=?", ws.WorkspaceID).LoadOne(&beforeRow))
	updated, err = svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Logo: &longLogo})
	require.NoError(t, err)
	var afterRow struct {
		UpdatedAt time.Time `db:"updated_at"`
	}
	require.NoError(t, ctx.DB().Select("updated_at").From("octo_workspace").
		Where("workspace_id=?", ws.WorkspaceID).LoadOne(&afterRow))
	require.Equal(t, beforeRow.UpdatedAt, afterRow.UpdatedAt)

	clearDescription := ""
	clearLogo := ""
	updated, err = svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Description: &clearDescription, Logo: &clearLogo})
	require.NoError(t, err)
	require.Empty(t, updated.Description)
	require.Empty(t, updated.Logo)
	// Same-value PUT is an idempotent success rather than a conflict or a
	// whole-object reset.
	updated, err = svc.Update(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.UpdateRequest{Description: &clearDescription})
	require.NoError(t, err)
	require.Equal(t, "Renamed", updated.Name)
}

func TestWorkspaceServiceBatchAtomicAndReadd(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-valid", "Valid candidate"},
		{"ws-outsider", "Outside candidate"},
		{"ws-disabled", "Disabled candidate"},
		{"ws-destroyed", "Destroyed candidate"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "ws-batch-space", "10000", "ws-valid", "ws-disabled", "ws-destroyed")
	seedWorkspaceSpace(t, ctx, "ws-other-space", "ws-outsider")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-batch-space", "Batch")

	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{
		{UID: "ws-valid", WorkspaceRole: workspacemod.WorkspaceRoleMember},
		{UID: "ws-outsider", WorkspaceRole: workspacemod.WorkspaceRoleMember},
	})
	require.ErrorIs(t, err, workspacemod.ErrCandidateIneligible)
	members, err := svc.Members(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.MemberFilter{Status: workspacemod.MemberStatusActive}, workspacemod.Page{Index: 1, Size: 200})
	require.NoError(t, err)
	require.EqualValues(t, 1, members.Count, "a failed batch must not partially admit its first candidate")
	_, err = ctx.DB().Update("user").Set("status", 0).Where("uid=?", "ws-disabled").Exec()
	require.NoError(t, err)
	_, err = ctx.DB().Update("user").Set("is_destroy", 2).Where("uid=?", "ws-destroyed").Exec()
	require.NoError(t, err)
	for _, candidate := range []string{"ws-disabled", "ws-destroyed"} {
		_, err = svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{
			{UID: "ws-valid", WorkspaceRole: workspacemod.WorkspaceRoleMember},
			{UID: candidate, WorkspaceRole: workspacemod.WorkspaceRoleMember},
		})
		require.ErrorIs(t, err, workspacemod.ErrCandidateIneligible)
		members, err = svc.Members(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.MemberFilter{Status: workspacemod.MemberStatusActive}, workspacemod.Page{Index: 1, Size: 200})
		require.NoError(t, err)
		require.EqualValues(t, 1, members.Count, "disabled/destroyed candidate must not partially admit a batch")
	}

	added, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{UID: "ws-valid", WorkspaceRole: workspacemod.WorkspaceRoleMember}})
	require.NoError(t, err)
	require.Len(t, added, 1)
	added, err = svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{UID: "ws-valid", WorkspaceRole: workspacemod.WorkspaceRoleMember}})
	require.NoError(t, err)
	require.Len(t, added, 1, "same-role re-add is an idempotent no-op")
	require.Equal(t, workspacemod.WorkspaceRoleMember, added[0].WorkspaceRole)

	_, err = svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{UID: "ws-valid", WorkspaceRole: workspacemod.WorkspaceRoleAdmin}})
	require.ErrorIs(t, err, workspacemod.ErrRoleInvalid, "role changes must use the dedicated PUT operation")
	member, err := svc.Member(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-valid")
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceRoleMember, member.WorkspaceRole)
}

func TestWorkspaceServiceTransferRejectsRevokedTarget(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceUser(t, ctx, "ws-revoked-target", "Revoked target")
	seedWorkspaceSpace(t, ctx, "ws-revoked-space", "10000", "ws-revoked-target")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-revoked-space", "Revoked target")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-revoked-target", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)
	_, err = ctx.DB().Update("space_member").Set("status", 0).
		Where("space_id=? AND uid=?", "ws-revoked-space", "ws-revoked-target").Exec()
	require.NoError(t, err)

	_, err = svc.TransferOwner(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-revoked-target")
	require.ErrorIs(t, err, workspacemod.ErrCandidateIneligible)
	owner, err := svc.Get(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID)
	require.NoError(t, err)
	require.Equal(t, "10000", owner.OwnerUID)
	require.Equal(t, workspacemod.WorkspaceRoleOwner, owner.WorkspaceRole)
}
func TestWorkspaceServicePaginationOverflowReturnsEmptyPage(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-page-space", "10000")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-page-space", "Page")

	page, err := svc.List(workspacemod.Scope{UID: "10000"}, "ws-page-space", "", workspacemod.Page{Index: math.MaxInt, Size: 200})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.Count)
	require.NotNil(t, page.List)
	require.Empty(t, page.List)

	members, err := svc.Members(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, workspacemod.MemberFilter{Status: workspacemod.MemberStatusActive}, workspacemod.Page{Index: math.MaxInt, Size: 200})
	require.NoError(t, err)
	require.EqualValues(t, 1, members.Count)
	require.NotNil(t, members.List)
	require.Empty(t, members.List)
}
func TestWorkspaceServiceReadsCompleteWithOneDatabaseConnection(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-one-connection-space", "10000")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-one-connection-space", "One connection")

	t.Cleanup(func() {
		bindWorkspaceTestDBPool(ctx)
	})
	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)

	list, err := svc.List(workspacemod.Scope{UID: "10000"}, "ws-one-connection-space", "", workspacemod.Page{Index: 1, Size: 15})
	require.NoError(t, err)
	require.EqualValues(t, 1, list.Count)
	require.Len(t, list.List, 1)

	detail, err := svc.Get(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID)
	require.NoError(t, err)
	require.Equal(t, ws.WorkspaceID, detail.WorkspaceID)

	members, err := svc.Members(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID,
		workspacemod.MemberFilter{Status: workspacemod.MemberStatusActive}, workspacemod.Page{Index: 1, Size: 15})
	require.NoError(t, err)
	require.EqualValues(t, 1, members.Count)
	require.Len(t, members.List, 1)
}
func TestWorkspaceServiceMemberWritesCompleteWithOneDatabaseConnection(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceUser(t, ctx, "ws-one-connection-member", "One connection member")
	seedWorkspaceSpace(t, ctx, "ws-one-connection-write-space", "10000", "ws-one-connection-member")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-one-connection-write-space", "One connection writes")

	t.Cleanup(func() {
		bindWorkspaceTestDBPool(ctx)
	})
	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)

	added, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-one-connection-member", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)
	require.Len(t, added, 1)

	updated, err := svc.UpdateMember(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID,
		"ws-one-connection-member", workspacemod.WorkspaceRoleAdmin)
	require.NoError(t, err)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, updated.WorkspaceRole)
}

func TestWorkspaceWritesDifferentWorkspacesShareSpaceLock(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-shared-write-space", "10000")
	svc := workspaceService(t, ctx)
	first := createWorkspaceService(t, ctx, "10000", "ws-shared-write-space", "First")
	second := createWorkspaceService(t, ctx, "10000", "ws-shared-write-space", "Second")

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	var heldSpace struct {
		SpaceID string `db:"space_id"`
	}
	require.NoError(t, tx.SelectBySql(
		"SELECT space_id FROM `space` WHERE space_id=? FOR SHARE", "ws-shared-write-space",
	).LoadOne(&heldSpace))
	require.Equal(t, "ws-shared-write-space", heldSpace.SpaceID)
	var heldWorkspace struct {
		WorkspaceID string `db:"workspace_id"`
	}
	require.NoError(t, tx.SelectBySql(
		"SELECT workspace_id FROM `octo_workspace` WHERE workspace_id=? FOR UPDATE", first.WorkspaceID,
	).LoadOne(&heldWorkspace))
	require.Equal(t, first.WorkspaceID, heldWorkspace.WorkspaceID)

	done := make(chan error, 1)
	go func() {
		name := "Second updated"
		_, updateErr := svc.Update(workspacemod.Scope{UID: "10000"}, second.WorkspaceID,
			workspacemod.UpdateRequest{Name: &name})
		done <- updateErr
	}()
	select {
	case updateErr := <-done:
		require.NoError(t, updateErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "a write to another Workspace waited on the held shared Space lock")
	}
	require.NoError(t, tx.Commit())
}

func TestWorkspaceGetsDoNotWaitOnLockedWorkspaceRows(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-locked-get-space", "10000")
	svc := workspaceService(t, ctx)
	first := createWorkspaceService(t, ctx, "10000", "ws-locked-get-space", "First")
	second := createWorkspaceService(t, ctx, "10000", "ws-locked-get-space", "Second")

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	_, err = tx.Update("octo_workspace").Set("name", "uncommitted").
		Where("workspace_id=?", first.WorkspaceID).Exec()
	require.NoError(t, err)

	type getResult struct {
		workspace *workspacemod.Workspace
		err       error
	}
	done := make(chan getResult, 2)
	go func() {
		workspace, getErr := svc.Get(workspacemod.Scope{UID: "10000"}, first.WorkspaceID)
		done <- getResult{workspace: workspace, err: getErr}
	}()
	go func() {
		workspace, getErr := svc.Get(workspacemod.Scope{UID: "10000"}, second.WorkspaceID)
		done <- getResult{workspace: workspace, err: getErr}
	}()

	for range 2 {
		select {
		case result := <-done:
			require.NoError(t, result.err)
			require.NotNil(t, result.workspace)
			if result.workspace.WorkspaceID == first.WorkspaceID {
				require.Equal(t, "First", result.workspace.Name,
					"GET must use the last committed Workspace state")
			} else {
				require.Equal(t, second.WorkspaceID, result.workspace.WorkspaceID)
			}
		case <-time.After(2 * time.Second):
			require.Fail(t, "a GET waited on an unrelated Workspace row lock")
		}
	}
	require.NoError(t, tx.Rollback())
}

func TestWorkspaceGroupAccessCurrentReadSeesMemberAfterPinnedSnapshot(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceUser(t, ctx, "ws-pinned-initial", "Initial member")
	seedWorkspaceUser(t, ctx, "ws-pinned-late", "Late member")
	seedWorkspaceSpace(t, ctx, "ws-pinned-snapshot-space", "10000", "ws-pinned-initial", "ws-pinned-late")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-pinned-snapshot-space", "Pinned snapshot")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-pinned-initial", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	var pinned struct {
		WorkspaceID string `db:"workspace_id"`
	}
	require.NoError(t, tx.SelectBySql(
		"SELECT workspace_id FROM `octo_workspace_member` WHERE workspace_id=? AND status=1",
		ws.WorkspaceID,
	).LoadOne(&pinned))
	require.Equal(t, ws.WorkspaceID, pinned.WorkspaceID)

	_, err = svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-pinned-late", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)

	access, err := svc.LockAccessesForGroupTx(tx, "10000", ws.WorkspaceID, []workspacemod.SpaceSeatKey{
		{SpaceID: "ws-pinned-snapshot-space", UID: "10000"},
		{SpaceID: "ws-pinned-snapshot-space", UID: "ws-pinned-initial"},
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"10000", "ws-pinned-initial", "ws-pinned-late"}, access.MemberUIDs,
		"current member read must not reuse the pinned repeatable-read snapshot")
	require.ElementsMatch(t, []string{"10000", "ws-pinned-initial"}, access.EligibleMemberUIDs,
		"only prepared seats may enter the eligible snapshot subset")
	require.NoError(t, tx.Commit())
}
func TestWorkspaceWriteAuthorizationUsesOwnerFromLockedWorkspaceRow(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Original owner")
	seedWorkspaceUser(t, ctx, "ws-owner-race-target", "New owner")
	seedWorkspaceSpace(t, ctx, "ws-owner-race-space", "10000", "ws-owner-race-target")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-owner-race-space", "Owner race")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-owner-race-target", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	var pinned struct {
		OwnerUID string `db:"owner_uid"`
	}
	require.NoError(t, tx.SelectBySql(
		"SELECT owner_uid FROM `octo_workspace` WHERE workspace_id=?", ws.WorkspaceID,
	).LoadOne(&pinned))
	require.Equal(t, "10000", pinned.OwnerUID)

	_, err = svc.TransferOwner(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-owner-race-target")
	require.NoError(t, err)

	accesses, err := svc.LockAccessesTx(tx, "10000", []string{ws.WorkspaceID}, "ws-owner-race-space")
	require.NoError(t, err,
		"authorization must use the owner from the locked Workspace row, not the pinned location read")
	access, ok := accesses[ws.WorkspaceID]
	require.True(t, ok)
	require.Equal(t, "ws-owner-race-target", access.OwnerUID)
	require.Equal(t, workspacemod.WorkspaceRoleAdmin, access.Role)
	require.NoError(t, tx.Commit())
}

func TestWorkspaceServiceWorkspaceNameSearchTreatsLikeMetacharactersLiterally(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceSpace(t, ctx, "ws-like-space", "10000")
	svc := workspaceService(t, ctx)
	for _, name := range []string{"literal %_! name", `literal \ name`, "literal ' name", "字面 %_ 名称", "literal ordinary name"} {
		_, err := svc.Create(workspacemod.Scope{UID: "10000"}, workspacemod.CreateRequest{
			SpaceID: "ws-like-space",
			Name:    name,
		})
		require.NoError(t, err)
	}

	ctx.DB().SetMaxOpenConns(1)
	ctx.DB().SetMaxIdleConns(1)
	var originalMode string
	require.NoError(t, ctx.DB().SelectBySql("SELECT @@SESSION.sql_mode").LoadOne(&originalMode))
	t.Cleanup(func() {
		_, restoreErr := ctx.DB().Exec("SET SESSION sql_mode = ?", originalMode)
		require.NoError(t, restoreErr)
		bindWorkspaceTestDBPool(ctx)
	})
	modeParts := make([]string, 0)
	for _, part := range strings.Split(originalMode, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !strings.EqualFold(part, "NO_BACKSLASH_ESCAPES") {
			modeParts = append(modeParts, part)
		}
	}
	defaultMode := strings.Join(modeParts, ",")
	noBackslashMode := defaultMode
	if noBackslashMode != "" {
		noBackslashMode += ","
	}
	noBackslashMode += "NO_BACKSLASH_ESCAPES"
	setMode := func(mode string) {
		_, err := ctx.DB().Exec("SET SESSION sql_mode = ?", mode)
		require.NoError(t, err)
	}

	for _, mode := range []string{defaultMode, noBackslashMode} {
		setMode(mode)
		for _, search := range []struct {
			keyword string
			name    string
		}{
			{keyword: "literal %_! name", name: "literal %_! name"},
			{keyword: `literal \ name`, name: `literal \ name`},
			{keyword: "literal ' name", name: "literal ' name"},
			{keyword: "字面 %_ 名称", name: "字面 %_ 名称"},
		} {
			page, err := svc.List(workspacemod.Scope{UID: "10000"}, "ws-like-space", search.keyword,
				workspacemod.Page{Index: 1, Size: 15})
			require.NoError(t, err)
			require.EqualValues(t, 1, page.Count, "mode %q keyword %q", mode, search.keyword)
			require.Len(t, page.List, 1, "mode %q keyword %q", mode, search.keyword)
			require.Equal(t, search.name, page.List[0].Name)
		}
	}
}

func TestWorkspaceLockAccessesForGroupLocksOnlyPreparedSeats(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-snapshot-member", "Snapshot member"},
		{"ws-unrelated-member", "Unrelated member"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "ws-snapshot-space", "10000", "ws-snapshot-member", "ws-unrelated-member")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-snapshot-space", "Snapshot")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-snapshot-member", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	var plan struct {
		Key *string `db:"key"`
	}
	require.NoError(t, tx.SelectBySql(
		"EXPLAIN SELECT space_id, uid FROM `space_member` WHERE status=1 AND (space_id, uid) IN ((?, ?), (?, ?)) ORDER BY space_id, uid FOR SHARE",
		"ws-snapshot-space", "10000", "ws-snapshot-space", "ws-snapshot-member",
	).LoadOne(&plan))
	require.NotNil(t, plan.Key)
	require.Equal(t, "spacemember_spaceid_uid", *plan.Key,
		"prepared seat locking must use the exact (space_id, uid) index")
	access, err := svc.LockAccessesForGroupTx(tx, "10000", ws.WorkspaceID, []workspacemod.SpaceSeatKey{
		{SpaceID: "ws-snapshot-space", UID: "10000"},
		{SpaceID: "ws-snapshot-space", UID: "ws-snapshot-member"},
	})
	require.NoError(t, err)
	require.Equal(t, ws.WorkspaceID, access.WorkspaceID)
	require.Equal(t, "ws-snapshot-space", access.SpaceID)
	require.ElementsMatch(t, []string{"10000", "ws-snapshot-member"}, access.MemberUIDs)

	type updateResult struct {
		affected int64
		err      error
	}
	updateDone := make(chan updateResult, 1)
	go func() {
		result, updateErr := ctx.DB().Update("space_member").Set("role", 1).
			Where("space_id=? AND uid=?", "ws-snapshot-space", "ws-unrelated-member").Exec()
		if updateErr != nil {
			updateDone <- updateResult{err: updateErr}
			return
		}
		affected, affectedErr := result.RowsAffected()
		updateDone <- updateResult{affected: affected, err: affectedErr}
	}()
	select {
	case result := <-updateDone:
		require.NoError(t, result.err)
		require.EqualValues(t, 1, result.affected,
			"prepared seat update must affect the unrelated member row")
		var updatedRole int
		require.NoError(t, ctx.DB().Select("role").From("space_member").
			Where("space_id=? AND uid=?", "ws-snapshot-space", "ws-unrelated-member").
			LoadOne(&updatedRole))
		require.Equal(t, 1, updatedRole)
	case <-time.After(2 * time.Second):
		require.Fail(t, "group snapshot locked an unrelated Space member")
	}
	require.NoError(t, tx.Commit())
}

func TestWorkspaceServiceTransferRemoveRacePreservesSingleOwner(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	for _, user := range []struct {
		uid  string
		name string
	}{
		{"10000", "Workspace owner"},
		{"ws-race-target", "Race target"},
	} {
		seedWorkspaceUser(t, ctx, user.uid, user.name)
	}
	seedWorkspaceSpace(t, ctx, "ws-race-space", "10000", "ws-race-target")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-race-space", "Race")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{UID: "ws-race-target", WorkspaceRole: workspacemod.WorkspaceRoleMember}})
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	go func() {
		defer wg.Done()
		_, err := svc.TransferOwner(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-race-target")
		results <- err
	}()
	go func() {
		defer wg.Done()
		results <- svc.RemoveMember(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-race-target")
	}()
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			require.True(t, errors.Is(err, workspacemod.ErrOwnerProtected) ||
				errors.Is(err, workspacemod.ErrCandidateIneligible) ||
				errors.Is(err, workspacemod.ErrNotFound), "race loser returned unexpected error: %v", err)
		}
	}

	owners, err := svc.Members(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID,
		workspacemod.MemberFilter{Roles: []string{workspacemod.WorkspaceRoleOwner}, Status: workspacemod.MemberStatusActive},
		workspacemod.Page{Index: 1, Size: 200})
	require.NoError(t, err)
	require.EqualValues(t, 1, owners.Count, "transfer/remove race must never expose zero or two owners")
	require.Len(t, owners.List, 1)
	assert.Contains(t, []string{"10000", "ws-race-target"}, owners.List[0].UID)
}

func TestWorkspaceServiceTransferWaitsForMemberLock(t *testing.T) {
	_, ctx := setupWorkspaceTest(t)
	seedWorkspaceUser(t, ctx, "10000", "Workspace owner")
	seedWorkspaceUser(t, ctx, "ws-lock-target", "Lock target")
	seedWorkspaceSpace(t, ctx, "ws-lock-space", "10000", "ws-lock-target")
	svc := workspaceService(t, ctx)
	ws := createWorkspaceService(t, ctx, "10000", "ws-lock-space", "Lock barrier")
	_, err := svc.AddMembers(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, []workspacemod.MemberInput{{
		UID: "ws-lock-target", WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)

	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	defer tx.RollbackUnlessCommitted()
	var locked struct {
		UID string `db:"uid"`
	}
	require.NoError(t, tx.SelectBySql(
		"SELECT uid FROM octo_workspace_member WHERE workspace_id=? AND uid=? FOR UPDATE",
		ws.WorkspaceID, "ws-lock-target",
	).LoadOne(&locked))
	require.Equal(t, "ws-lock-target", locked.UID)

	done := make(chan error, 1)
	go func() {
		_, transferErr := svc.TransferOwner(workspacemod.Scope{UID: "10000"}, ws.WorkspaceID, "ws-lock-target")
		done <- transferErr
	}()
	select {
	case transferErr := <-done:
		require.Failf(t, "transfer bypassed member row lock", "transfer returned before the lock barrier released: %v", transferErr)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	select {
	case transferErr := <-done:
		require.NoError(t, transferErr)
	case <-time.After(5 * time.Second):
		require.Fail(t, "transfer remained blocked after member lock release")
	}
}
