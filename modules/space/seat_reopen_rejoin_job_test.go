package space

// 重新打开一个 Space 席位不得再写出 reason=rejoined 的专属投影工单：那条链路已
// 停用，关席位写出的原生清理工单（kicked 等）仍独立产生、独立等待 worker。
// 「重新打开方向照旧触发 Project epoch 失效步骤」由 modules/project 的
// space_rejoin_epoch_test.go 在真表上钉住；本包只断言能直接观察到的库内事实。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeatReopenEnqueuesNoRejoinJob(t *testing.T) {
	_, f, err := setup(t)
	require.NoError(t, err)

	const (
		spaceID = "wk-seat-reopen-rejoin"
		uid     = "seat-reopen-target"
		owner   = "seat-reopen-owner"
	)
	seedMember(t, f, spaceID, owner, 2)
	require.NoError(t, f.db.insertMemberNoTx(&MemberModel{
		SpaceId: spaceID, UID: uid, Role: 0, Status: 1,
	}))

	// 前提：经真实的关席位事务把人踢掉，原生清理工单必须照常写出。
	mustRemoveMember(t, f, spaceID, uid, 1, owner, MemberRemoveReasonKicked)
	before := cleanupJobs(t, spaceID)
	require.Len(t, before, 1, "关席位必须写出原生清理工单")
	require.Equal(t, MemberRemoveReasonKicked, before[0].Reason)

	// 重新打开走 POST /v1/space/:space_id/members/add 使用的同一个事务
	// （api.go 的 reactivateMember 分支）。
	require.NoError(t, f.db.reactivateMember(spaceID, uid, 0))

	status, found := memberStatus(t, spaceID, uid)
	require.True(t, found, "重新打开翻转既有席位，不得变成新插入")
	assert.Equal(t, 1, status, "重新打开后席位必须活跃")

	after := cleanupJobs(t, spaceID)
	require.Len(t, after, 1, "重新打开不得入队工单：旧实现会多出一行 reason=rejoined")
	assert.Equal(t, MemberRemoveReasonKicked, after[0].Reason,
		"关席位的原生清理工单独立存在，重新打开不得消费或改写它")
}
