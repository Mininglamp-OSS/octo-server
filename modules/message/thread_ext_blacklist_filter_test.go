package message

// =============================================================================
// Issue #351（PR #345 mandatory follow-up）— 子区 ext 物化按活跃成员过滤。
//
// AuthorizeChannelFollow 对 GROUP follow 保持 permissive ExistMember，但
// FollowChannel 会物化既有子区 ext 行、OnThreadCreated fanout 会给所有
// auto_follow_threads=1 的群行物化新子区 ext 行。被拉黑（status=Blacklist、
// is_deleted=0）的父群成员两条路径都不应再收到子区 ext 行（元数据/通知层泄漏；
// 内容读已被 ExistMemberActive 门禁兜住）。
//
// 非 integration tag：CI 的 go test 直接跑（MySQL/Redis service 已就绪），
// 避免 #353 指出的「integration-tagged 测试 CI 永不编译」缺口在本修复上重演。
// =============================================================================

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extBlSpaceID = "s_ext_bl"

// setupThreadExtBlacklistData 建一个父群（space_id 为空 → legacy wildcard 可见），
// 两个正常成员 normalUID / victimUID，返回 ctx 与已装配好的 conversation_ext 单例。
// 装配（ThreadAuthChecker / ChannelAuthChecker / ThreadEnumerator / ActiveMemberFilter）
// 由 testutil.NewTestServer → module.Setup 跑本包 1module.go 的注入逻辑完成，
// 与生产 boot 同一条 wiring 路径。
func setupThreadExtBlacklistData(t *testing.T) (*config.Context, *convext.Service, string, string, string) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	svc := convext.GetGlobalConvExtService()
	require.NotNil(t, svc, "conversation_ext 单例应已由 module.Setup 初始化并完成注入")

	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	normalUID := "u_ext_normal_" + util.GenerUUID()[:8]
	victimUID := "u_ext_victim_" + util.GenerUUID()[:8]

	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{
		GroupNo: groupNo, Name: "父群", Creator: normalUID, Status: 1, Version: 1,
	}))
	for _, u := range []string{normalUID, victimUID} {
		require.NoError(t, groupDB.InsertMember(&group.MemberModel{
			GroupNo: groupNo, UID: u,
			Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
		}))
	}
	return ctx, svc, groupNo, normalUID, victimUID
}

func blacklistMemberExtBl(t *testing.T, ctx *config.Context, groupNo, uid string) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), groupNo, uid,
	).Exec()
	require.NoError(t, err)
}

// hasThreadExtRow 查 user_conversation_ext 是否存在 (uid, space, target_type=5, channelID) 行。
func hasThreadExtRow(t *testing.T, ctx *config.Context, uid, channelID string) bool {
	t.Helper()
	var count int64
	_, err := ctx.DB().Select("count(*)").From("user_conversation_ext").
		Where("uid=? AND space_id=? AND target_type=5 AND target_id=?", uid, extBlSpaceID, channelID).
		Load(&count)
	require.NoError(t, err)
	return count > 0
}

// seedActiveThread 在 thread 表插入一个 active 子区，返回 channelID。
func seedActiveThread(t *testing.T, ctx *config.Context, groupNo, shortID string) string {
	t.Helper()
	require.NoError(t, thread.NewDB(ctx).Insert(&thread.Model{
		ShortID: shortID, GroupNo: groupNo, Name: "topic", CreatorUID: "creator",
		Status: thread.ThreadStatusActive,
	}))
	return groupNo + "____" + shortID
}

// TestOnThreadCreated_BlacklistedMemberExcludedFromFanout 验证：两个成员都开启了
// auto_follow_threads（正常时 FollowChannel），其中一个随后被拉黑——新建子区的
// fanout 只给活跃成员物化 ext 行；解除拉黑后 fanout 自动恢复。
func TestOnThreadCreated_BlacklistedMemberExcludedFromFanout(t *testing.T) {
	ctx, svc, groupNo, normalUID, victimUID := setupThreadExtBlacklistData(t)

	// 两人在正常状态下关注 channel（写 auto_follow_threads=1 群行）。
	require.NoError(t, svc.FollowChannel(normalUID, extBlSpaceID, groupNo))
	require.NoError(t, svc.FollowChannel(victimUID, extBlSpaceID, groupNo))

	// victim 被拉黑后新建子区。
	blacklistMemberExtBl(t, ctx, groupNo, victimUID)
	sid1 := "1489104291682713601"
	require.NoError(t, svc.OnThreadCreated(groupNo, sid1))

	assert.True(t, hasThreadExtRow(t, ctx, normalUID, groupNo+"____"+sid1),
		"正常成员应收到新子区 ext 行")
	assert.False(t, hasThreadExtRow(t, ctx, victimUID, groupNo+"____"+sid1),
		"被拉黑成员不应收到新子区 ext 行（issue #351 元数据泄漏）")

	// 解除拉黑 → 下一条新子区恢复 fanout（auto_follow_threads=1 保留的语义）。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusNormal), groupNo, victimUID,
	).Exec()
	require.NoError(t, err)
	sid2 := "1489104291682713602"
	require.NoError(t, svc.OnThreadCreated(groupNo, sid2))
	assert.True(t, hasThreadExtRow(t, ctx, victimUID, groupNo+"____"+sid2),
		"解除拉黑后 fanout 应自动恢复")
}

// TestFollowChannel_BlacklistedMemberSkipsExistingThreadMaterialization 验证：
// 被拉黑成员调用 FollowChannel（GROUP 门禁 permissive，调用本身放行）时，
// 既有子区的 ext 物化被跳过；正常成员则正常物化。
func TestFollowChannel_BlacklistedMemberSkipsExistingThreadMaterialization(t *testing.T) {
	ctx, svc, groupNo, normalUID, victimUID := setupThreadExtBlacklistData(t)

	existingChannelID := seedActiveThread(t, ctx, groupNo, "1489104291682713603")

	blacklistMemberExtBl(t, ctx, groupNo, victimUID)

	// 被拉黑成员 FollowChannel：GROUP 行写入放行（permissive 语义不变），但
	// 不得物化既有子区 ext 行。
	require.NoError(t, svc.FollowChannel(victimUID, extBlSpaceID, groupNo),
		"GROUP follow 对被拉黑成员保持 permissive，调用不应报错")
	assert.False(t, hasThreadExtRow(t, ctx, victimUID, existingChannelID),
		"被拉黑成员不应通过 FollowChannel 重新物化既有子区 ext 行（issue #351）")

	var groupRowCount int64
	_, err := ctx.DB().Select("count(*)").From("user_conversation_ext").
		Where("uid=? AND space_id=? AND target_type=2 AND target_id=?",
			victimUID, extBlSpaceID, groupNo).
		Load(&groupRowCount)
	require.NoError(t, err)
	assert.Equal(t, int64(1), groupRowCount, "GROUP 级 ext 行本身应照常写入（permissive 语义不变）")

	// 对照组：正常成员 FollowChannel 正常物化既有子区。
	require.NoError(t, svc.FollowChannel(normalUID, extBlSpaceID, groupNo))
	assert.True(t, hasThreadExtRow(t, ctx, normalUID, existingChannelID),
		"正常成员 FollowChannel 应物化既有子区 ext 行")
}
