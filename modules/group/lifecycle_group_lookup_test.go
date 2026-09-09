package group

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/require"
)

func TestManagedGroupsDoNotConsumeDailyGroupCreationQuota(t *testing.T) {
	_, _, ctx := setupServiceTestWithCtx(t)
	const ownerUID = "managed_quota_owner"

	for _, model := range []*Model{
		{GroupNo: "ordinary_quota_group", Name: "ordinary", Creator: ownerUID, Status: GroupStatusNormal},
		{GroupNo: "agent_quota_group", Name: "agent", Creator: ownerUID, Status: GroupStatusNormal, Purpose: aiteam.GroupPurpose},
		{GroupNo: "team_quota_group", Name: "team", Creator: ownerUID, Status: GroupStatusNormal, Purpose: aiteam.TeamGroupPurpose},
	} {
		require.NoError(t, NewDB(ctx).Insert(model))
	}

	count, err := NewDB(ctx).querySameDayCreateCountWitUID(ownerUID, util.Toyyyy_MM_dd(time.Now()))
	require.NoError(t, err)
	require.Equal(t, 1, count, "only manually-created groups consume the user's daily quota")
}

func TestGetGroupsWithMemberUIDForLifecycleCleanupIncludesAITeamContainers(t *testing.T) {
	svc, _, ctx := setupServiceTestWithCtx(t)
	const botUID = "bot_lifecycle_lookup"

	for _, model := range []*Model{
		{GroupNo: "ordinary_lifecycle_lookup", Name: "ordinary", Creator: "owner", Status: GroupStatusNormal},
		{GroupNo: "ai_lifecycle_lookup", Name: "ai", Creator: "owner", Status: GroupStatusNormal, Purpose: aiteam.GroupPurpose},
		{GroupNo: "ai_team_lifecycle_lookup", Name: "ai team", Creator: "owner", Status: GroupStatusNormal, Purpose: aiteam.TeamGroupPurpose},
	} {
		require.NoError(t, NewDB(ctx).Insert(model))
		require.NoError(t, NewDB(ctx).InsertMember(&MemberModel{
			GroupNo: model.GroupNo, UID: botUID, Status: int(common.GroupMemberStatusNormal), Robot: 1, Version: 1,
		}))
	}

	productGroups, err := svc.GetGroupsWithMemberUID(botUID)
	require.NoError(t, err)
	require.Len(t, productGroups, 1)
	require.Equal(t, "ordinary_lifecycle_lookup", productGroups[0].GroupNo)

	lifecycleGroups, err := svc.GetGroupsWithMemberUIDForLifecycleCleanup(botUID)
	require.NoError(t, err)
	require.Len(t, lifecycleGroups, 3)
	require.ElementsMatch(t,
		[]string{"ordinary_lifecycle_lookup", "ai_lifecycle_lookup", "ai_team_lifecycle_lookup"},
		[]string{lifecycleGroups[0].GroupNo, lifecycleGroups[1].GroupNo, lifecycleGroups[2].GroupNo},
	)
}

func TestRemoveUserFromGroupsForLifecycleCleanupSkipsGroupCreator(t *testing.T) {
	svc, _, ctx := setupServiceTestWithCtx(t)
	const botUID = "bot_lifecycle_creator"
	const groupNo = "group_lifecycle_creator"

	require.NoError(t, NewDB(ctx).Insert(&Model{
		GroupNo: groupNo, Name: "bot-owned legacy group", Creator: botUID, Status: GroupStatusNormal,
	}))
	require.NoError(t, NewDB(ctx).InsertMember(&MemberModel{
		GroupNo: groupNo, UID: botUID, Role: MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Robot: 1, Version: 1,
	}))

	require.NoError(t, svc.RemoveUserFromGroupsForLifecycleCleanup(botUID))
	member, err := NewDB(ctx).QueryMemberWithUID(botUID, groupNo)
	require.NoError(t, err)
	require.NotNil(t, member)
	require.Equal(t, MemberRoleCreator, member.Role)
	require.Zero(t, member.IsDeleted, "creator membership is intentionally retained without failing teardown")
}
