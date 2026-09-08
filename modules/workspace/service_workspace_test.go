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
