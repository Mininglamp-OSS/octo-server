package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/require"
)

func TestGetGroupsWithMemberUIDForLifecycleCleanupIncludesAITeamContainers(t *testing.T) {
	svc, _, ctx := setupServiceTestWithCtx(t)
	const botUID = "bot_lifecycle_lookup"

	for _, model := range []*Model{
		{GroupNo: "ordinary_lifecycle_lookup", Name: "ordinary", Creator: "owner", Status: GroupStatusNormal},
		{GroupNo: "ai_lifecycle_lookup", Name: "ai", Creator: "owner", Status: GroupStatusNormal, Purpose: aiteam.GroupPurpose},
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
	require.Len(t, lifecycleGroups, 2)
	require.ElementsMatch(t,
		[]string{"ordinary_lifecycle_lookup", "ai_lifecycle_lookup"},
		[]string{lifecycleGroups[0].GroupNo, lifecycleGroups[1].GroupNo},
	)
}
