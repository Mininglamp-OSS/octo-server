//go:build integration

package conversation_ext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue #557 — 创建子区后创建者自己看不到（关注 Tab 不出现）。
//
// 根因：子区能否进创建者的关注 Tab 只看 user_conversation_ext 里有没有该子区行。
// 落这行的两条现存路径都不覆盖「未关注父群的创建者」：
//   - CreateThread 只写 thread_member + IM subscribers，不写任何 ext 行；
//   - OnThreadCreated fanout 只给 auto_follow_threads=1 AND group_unfollowed=0
//     的成员补行，创建者不被特殊对待。
//
// 修复：新增 EnsureThreadFollowForCreator，在 thread 创建提交路径里无条件给创建者
// 补一条子区 ext 行，且**不触碰**父群的 group_unfollowed（octo-web #293 被拒的 P1）。

// Test_Issue557_OnThreadCreated_DoesNotCoverUnfollowedCreator 固化根因：仅靠现有
// fanout，一个「已显式取关父群」的创建者不会拿到子区 ext 行——这正是本 issue 的缺口。
func Test_Issue557_OnThreadCreated_DoesNotCoverUnfollowedCreator(t *testing.T) {
	svc := newServiceForTest(t)
	const creator, space, grp, shortID = "u-creator", "s-557a", "grp-557a", "thr-557a"
	channelID := grp + threadSeparator + shortID

	one := int8(1)
	// 创建者显式取关过父群：auto_follow=1 但 group_unfollowed=1（取关语义）。
	require.NoError(t, svc.db.Upsert(creator, space, targetTypeGroup, grp, ConvExtFields{
		AutoFollowThreads: &one,
		GroupUnfollowed:   &one,
	}))

	require.NoError(t, svc.OnThreadCreated(grp, shortID))

	row, err := svc.db.Get(creator, space, targetTypeThread, channelID)
	require.NoError(t, err)
	assert.Nil(t, row, "fanout 不应给已取关父群的创建者补子区行——正是本 issue 需要单独补齐的缺口")
}

// Test_Issue557_EnsureThreadFollowForCreator_WritesRowWhenNoParentGroupRow 覆盖核心
// 场景：创建者从未关注父群（无任何 ext 行）。补行后创建者应有子区行，且**不应**凭空
// 造出一条父群 follow 行。
func Test_Issue557_EnsureThreadFollowForCreator_WritesRowWhenNoParentGroupRow(t *testing.T) {
	svc := newServiceForTest(t)
	const creator, space, grp, shortID = "u-creator", "s-557b", "grp-557b", "thr-557b"
	channelID := grp + threadSeparator + shortID

	beforeVer := readFollowVersion(t, svc, creator, space)

	require.NoError(t, svc.EnsureThreadFollowForCreator(creator, space, channelID))

	threadRow, err := svc.db.Get(creator, space, targetTypeThread, channelID)
	require.NoError(t, err)
	require.NotNil(t, threadRow, "创建者应拿到自己新建子区的 ext 行")

	// 不得副作用地给创建者造一条父群 follow 行。
	groupRow, err := svc.db.Get(creator, space, targetTypeGroup, grp)
	require.NoError(t, err)
	assert.Nil(t, groupRow, "补创建者子区行不应凭空创建父群 follow 行")

	// follow_version 必须 +1，客户端才会刷新关注列表。
	assert.Greater(t, readFollowVersion(t, svc, creator, space), beforeVer,
		"补行应 bump follow_version 触发 sidebar 刷新")
}

// Test_Issue557_EnsureThreadFollowForCreator_DoesNotClearGroupUnfollowed 固化
// octo-web #293 被拒的 P1 边界：即便创建者显式取关过父群，补子区行也**不能**把
// 父群 group_unfollowed 清零（不得因创建子区把用户显式取关的父群拖回关注 Tab）。
func Test_Issue557_EnsureThreadFollowForCreator_DoesNotClearGroupUnfollowed(t *testing.T) {
	svc := newServiceForTest(t)
	const creator, space, grp, shortID = "u-creator", "s-557c", "grp-557c", "thr-557c"
	channelID := grp + threadSeparator + shortID

	one := int8(1)
	zero := int8(0)
	require.NoError(t, svc.db.Upsert(creator, space, targetTypeGroup, grp, ConvExtFields{
		AutoFollowThreads: &zero,
		GroupUnfollowed:   &one, // 已显式取关父群
	}))

	require.NoError(t, svc.EnsureThreadFollowForCreator(creator, space, channelID))

	threadRow, err := svc.db.Get(creator, space, targetTypeThread, channelID)
	require.NoError(t, err)
	require.NotNil(t, threadRow, "创建者仍应拿到子区行")

	groupRow, err := svc.db.Get(creator, space, targetTypeGroup, grp)
	require.NoError(t, err)
	require.NotNil(t, groupRow)
	assert.Equal(t, int8(1), groupRow.GroupUnfollowed,
		"父群 group_unfollowed 不得被清零（不应因创建子区把显式取关的父群拖回关注 Tab）")
}

// Test_Issue557_EnsureThreadFollowForCreator_Idempotent 校验幂等：重复调用（或与
// fanout 竞态）不产生脏行，也不覆盖用户已有的手动 follow_sort。
func Test_Issue557_EnsureThreadFollowForCreator_Idempotent(t *testing.T) {
	svc := newServiceForTest(t)
	const creator, space, grp, shortID = "u-creator", "s-557d", "grp-557d", "thr-557d"
	channelID := grp + threadSeparator + shortID

	// 预置：子区行已存在且带手动排序（模拟 fanout 先落行或用户已拖拽排序）。
	require.NoError(t, svc.db.Upsert(creator, space, targetTypeThread, channelID, ConvExtFields{
		FollowSort: intPtr(42),
	}))

	require.NoError(t, svc.EnsureThreadFollowForCreator(creator, space, channelID))
	require.NoError(t, svc.EnsureThreadFollowForCreator(creator, space, channelID))

	row, err := svc.db.Get(creator, space, targetTypeThread, channelID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, 42, row.FollowSort, "已有手动排序不应被补行覆盖（INSERT IGNORE 语义）")
}

// Test_Issue557_EnsureThreadFollowForCreator_RejectsBadInput 校验入参防御。
func Test_Issue557_EnsureThreadFollowForCreator_RejectsBadInput(t *testing.T) {
	svc := newServiceForTest(t)

	assert.Error(t, svc.EnsureThreadFollowForCreator("", "s1", "grp-1____thr-1"),
		"空 uid 应报错")
	assert.Error(t, svc.EnsureThreadFollowForCreator("u1", "s1", "no-separator"),
		"非法 threadChannelID 应报错")

	// 历史无 space 群：空 space_id 合法（与 fanout 口径一致，不复用 validateBase）。
	assert.NoError(t, svc.EnsureThreadFollowForCreator("u1", "", "grp-legacy____thr-1"),
		"legacy（无 space）群允许空 space_id")
}
