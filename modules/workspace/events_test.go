package workspace_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/base/outbox"
	workspacemod "github.com/Mininglamp-OSS/octo-server/modules/workspace"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/require"
)

const (
	workspaceEventLoopToken   = "workspace-event-loop-token-012345678901234567890"
	workspaceEventDriverToken = "workspace-event-driver-token-012345678901234567890"
)

type workspaceEventEnvelope struct {
	EventID    string         `json:"event_id"`
	Domain     string         `json:"domain"`
	ResourceID string         `json:"resource_id"`
	EventType  string         `json:"event_type"`
	Data       map[string]any `json:"data"`
}

type workspaceEventOutboxRow struct {
	EventID       string `db:"event_id"`
	Domain        string `db:"domain"`
	ResourceID    string `db:"resource_id"`
	EventType     string `db:"event_type"`
	TargetService string `db:"target_service"`
	Payload       string `db:"payload"`
	Status        uint8  `db:"status"`
}

func TestWorkspaceWriteEvents(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		run       func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string
	}{
		{
			name:      "Create",
			eventType: "workspace.created",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				seedWorkspaceUser(t, ctx, "event-create-owner", "Event create owner")
				seedWorkspaceSpace(t, ctx, "event-create-space", "event-create-owner")
				ws, err := svc.Create(workspacemod.Scope{UID: "event-create-owner"}, workspacemod.CreateRequest{
					SpaceID: "event-create-space",
					Name:    "Created workspace",
				})
				require.NoError(t, err)
				return ws.WorkspaceID
			},
		},
		{
			name:      "Update",
			eventType: "workspace.updated",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-update-space", "event-update-owner")
				name := "Updated workspace"
				_, err := svc.Update(workspacemod.Scope{UID: "event-update-owner"}, ws, workspacemod.UpdateRequest{Name: &name})
				require.NoError(t, err)
				return ws
			},
		},
		{
			name:      "AddMembers",
			eventType: "workspace.members_changed",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-add-space", "event-add-owner", "event-add-member")
				_, err := svc.AddMembers(workspacemod.Scope{UID: "event-add-owner"}, ws, []workspacemod.MemberInput{{
					UID:           "event-add-member",
					WorkspaceRole: workspacemod.WorkspaceRoleMember,
				}})
				require.NoError(t, err)
				return ws
			},
		},
		{
			name:      "UpdateMember",
			eventType: "workspace.members_changed",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-role-space", "event-role-owner", "event-role-member")
				_, err := svc.AddMembers(workspacemod.Scope{UID: "event-role-owner"}, ws, []workspacemod.MemberInput{{
					UID:           "event-role-member",
					WorkspaceRole: workspacemod.WorkspaceRoleMember,
				}})
				require.NoError(t, err)
				clearWorkspaceEventState(t, ctx)
				_, err = svc.UpdateMember(workspacemod.Scope{UID: "event-role-owner"}, ws, "event-role-member", workspacemod.WorkspaceRoleAdmin)
				require.NoError(t, err)
				return ws
			},
		},
		{
			name:      "RemoveMember",
			eventType: "workspace.members_changed",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-remove-space", "event-remove-owner", "event-remove-member")
				_, err := svc.AddMembers(workspacemod.Scope{UID: "event-remove-owner"}, ws, []workspacemod.MemberInput{{
					UID:           "event-remove-member",
					WorkspaceRole: workspacemod.WorkspaceRoleMember,
				}})
				require.NoError(t, err)
				clearWorkspaceEventState(t, ctx)
				require.NoError(t, svc.RemoveMember(workspacemod.Scope{UID: "event-remove-owner"}, ws, "event-remove-member"))
				return ws
			},
		},
		{
			name:      "Leave",
			eventType: "workspace.members_changed",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-leave-space", "event-leave-owner", "event-leave-member")
				_, err := svc.AddMembers(workspacemod.Scope{UID: "event-leave-owner"}, ws, []workspacemod.MemberInput{{
					UID:           "event-leave-member",
					WorkspaceRole: workspacemod.WorkspaceRoleMember,
				}})
				require.NoError(t, err)
				clearWorkspaceEventState(t, ctx)
				require.NoError(t, svc.Leave(workspacemod.Scope{UID: "event-leave-member"}, ws))
				return ws
			},
		},
		{
			name:      "TransferOwner",
			eventType: "workspace.owner_transferred",
			run: func(t *testing.T, ctx *config.Context, svc *workspacemod.Service) string {
				ws := createEventWorkspace(t, ctx, svc, "event-transfer-space", "event-transfer-owner", "event-transfer-target")
				_, err := svc.AddMembers(workspacemod.Scope{UID: "event-transfer-owner"}, ws, []workspacemod.MemberInput{{
					UID:           "event-transfer-target",
					WorkspaceRole: workspacemod.WorkspaceRoleMember,
				}})
				require.NoError(t, err)
				clearWorkspaceEventState(t, ctx)
				_, err = svc.TransferOwner(workspacemod.Scope{UID: "event-transfer-owner"}, ws, "event-transfer-target")
				require.NoError(t, err)
				return ws
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client := setupWorkspaceEventTest(t)
			svc := workspaceService(t, ctx)
			workspaceID := tt.run(t, ctx, svc)
			rows := loadWorkspaceEventRows(t, ctx, workspaceID, tt.eventType)
			require.Len(t, rows, 2)
			require.Equal(t, rows[0].EventID, rows[1].EventID)
			require.Equal(t, "workspace", rows[0].Domain)
			require.Equal(t, workspaceID, rows[0].ResourceID)
			require.Equal(t, tt.eventType, rows[0].EventType)
			require.Equal(t, uint8(1), rows[0].Status)
			require.Equal(t, uint8(1), rows[1].Status)
			require.ElementsMatch(t, []string{"loop", "driver"}, []string{rows[0].TargetService, rows[1].TargetService})

			for _, target := range []string{"loop", "driver"} {
				entries, err := client.XRange("event_queue:workspace:"+target, "-", "+").Result()
				require.NoError(t, err)
				require.Len(t, entries, 1)
				raw, ok := entries[0].Values["event_json"].(string)
				require.True(t, ok)
				var envelope workspaceEventEnvelope
				require.NoError(t, json.Unmarshal([]byte(raw), &envelope))
				require.Equal(t, rows[0].EventID, envelope.EventID)
				require.Equal(t, "workspace", envelope.Domain)
				require.Equal(t, workspaceID, envelope.ResourceID)
				require.Equal(t, tt.eventType, envelope.EventType)
				require.Equal(t, workspaceID, envelope.Data["workspace_id"])
				require.Len(t, envelope.Data, 1)
			}
		})
	}
}

func TestWorkspaceWriteEventsSkipNoopChanges(t *testing.T) {
	ctx, _ := setupWorkspaceEventTest(t)
	svc := workspaceService(t, ctx)
	ws := createEventWorkspace(t, ctx, svc, "event-noop-space", "event-noop-owner", "event-noop-member")

	clearWorkspaceEventState(t, ctx)
	name := "Workspace event workspace"
	_, err := svc.Update(workspacemod.Scope{UID: "event-noop-owner"}, ws, workspacemod.UpdateRequest{Name: &name})
	require.NoError(t, err)
	require.Zero(t, workspaceEventRowCount(t, ctx, ws, "workspace.updated"))

	_, err = svc.AddMembers(workspacemod.Scope{UID: "event-noop-owner"}, ws, []workspacemod.MemberInput{{
		UID:           "event-noop-member",
		WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)
	clearWorkspaceEventState(t, ctx)
	_, err = svc.AddMembers(workspacemod.Scope{UID: "event-noop-owner"}, ws, []workspacemod.MemberInput{{
		UID:           "event-noop-member",
		WorkspaceRole: workspacemod.WorkspaceRoleMember,
	}})
	require.NoError(t, err)
	require.Zero(t, workspaceEventRowCount(t, ctx, ws, "workspace.members_changed"))
}

func TestWorkspaceWriteEventsFailureLeavesNoOutboxRow(t *testing.T) {
	ctx, _ := setupWorkspaceEventTest(t)
	svc := workspaceService(t, ctx)
	ws := createEventWorkspace(t, ctx, svc, "event-failure-space", "event-failure-owner", "event-failure-member")
	clearWorkspaceEventState(t, ctx)

	_, err := svc.AddMembers(workspacemod.Scope{UID: "event-failure-owner"}, ws, []workspacemod.MemberInput{
		{UID: "event-failure-member", WorkspaceRole: workspacemod.WorkspaceRoleMember},
		{UID: "event-failure-outside", WorkspaceRole: workspacemod.WorkspaceRoleMember},
	})
	require.Error(t, err)
	require.Zero(t, workspaceEventRowCount(t, ctx, ws, "workspace.members_changed"))
	var memberCount int
	_, err = ctx.DB().Select("COUNT(*)").From("octo_workspace_member").
		Where("workspace_id=? AND uid=? AND status=1", ws, "event-failure-member").Load(&memberCount)
	require.NoError(t, err)
	require.Zero(t, memberCount, "the failed transaction must not admit any member")
}

func setupWorkspaceEventTest(t *testing.T) (*config.Context, *redis.Client) {
	t.Helper()
	t.Setenv(workspacemod.LoopInternalTokenEnv, workspaceEventLoopToken)
	t.Setenv(workspacemod.DriveInternalTokenEnv, workspaceEventDriverToken)
	t.Setenv("NOTIFY_INTERNAL_TOKEN", "")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", "")
	t.Setenv("OCTO_DOCS_BOT_MENTION_TOKEN", "")
	_, ctx := setupWorkspaceTest(t)
	outbox.Init(ctx)
	workspacemod.RegisterEventTargets()
	client := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	clearWorkspaceEventState(t, ctx)
	t.Cleanup(func() {
		_ = client.Del("event_queue:workspace:loop", "event_queue:workspace:driver").Err()
		_ = client.Close()
		resetWorkspaceUIDRateLimit(t, ctx)
	})
	return ctx, client
}

func createEventWorkspace(t *testing.T, ctx *config.Context, svc *workspacemod.Service, spaceID, ownerUID string, otherUIDs ...string) string {
	t.Helper()
	seedWorkspaceUser(t, ctx, ownerUID, fmt.Sprintf("User %s", ownerUID))
	uids := append([]string{ownerUID}, otherUIDs...)
	for _, uid := range otherUIDs {
		seedWorkspaceUser(t, ctx, uid, fmt.Sprintf("User %s", uid))
	}
	seedWorkspaceSpace(t, ctx, spaceID, uids...)
	ws, err := svc.Create(workspacemod.Scope{UID: ownerUID}, workspacemod.CreateRequest{SpaceID: spaceID, Name: "Workspace event workspace"})
	require.NoError(t, err)
	clearWorkspaceEventState(t, ctx)
	return ws.WorkspaceID
}

func clearWorkspaceEventState(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().DeleteFrom("event_outbox").Exec()
	require.NoError(t, err)
	client := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer client.Close()
	require.NoError(t, client.Del("event_queue:workspace:loop", "event_queue:workspace:driver").Err())
}

func loadWorkspaceEventRows(t *testing.T, ctx *config.Context, workspaceID, eventType string) []workspaceEventOutboxRow {
	t.Helper()
	var rows []workspaceEventOutboxRow
	_, err := ctx.DB().Select("event_id", "domain", "resource_id", "event_type", "target_service", "payload", "status").
		From("event_outbox").Where("domain=? AND resource_id=? AND event_type=?", "workspace", workspaceID, eventType).
		OrderAsc("id").Load(&rows)
	require.NoError(t, err)
	return rows
}

func workspaceEventRowCount(t *testing.T, ctx *config.Context, workspaceID, eventType string) int {
	t.Helper()
	var count int
	_, err := ctx.DB().Select("COUNT(*)").From("event_outbox").
		Where("domain=? AND resource_id=? AND event_type=?", "workspace", workspaceID, eventType).
		Load(&count)
	require.NoError(t, err)
	return count
}
