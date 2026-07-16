package thread

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Issue557_LegacyGroup_CreatorBackfillSurvivesSpaceScopedRead 覆盖 reviewer
// yujiawei 认定的 legacy-space P1（issue #557 的 silent no-op）。
//
// 场景：一个 legacy 群（group.space_id=''）里的内部成员创建子区。
//   - 写侧：GetMemberExternalFields 对 legacy 内部群返回 effective space ''（
//     group/service.go:515-517），若原样传给 EnsureThreadFollowForCreator，补行落
//     user_conversation_ext.space_id=''。
//   - 读侧：现代客户端读 legacy 群时带的是**非空** default space（#484 口径，见
//     message/space_filter.go:404-406 与 api_sidebar.go:896-902 的 legacy 分支只在
//     spaceID==defaultSpaceID 时露出），ListThreadExts 精确 `WHERE space_id=?`
//     （conversation_ext/db.go:238-245）在 SQL 层丢掉 space_id='' 的补行。
//
// 本测试直接覆盖真正丢行的 **SQL 过滤层**（而非 post-SQL 的 mergeThreadEntries——
// 那里永远收不到被 SQL 丢掉的行）。它只需 MySQL：不 require WuKongIM，也不碰
// GetGlobalConvExtService 单例，所以 `-shuffle=on` 下无条件确定性运行。
//
// 修复前：resolveCreatorBackfillSpaceID 返回 ''，补行落 ''，以非空 default 读丢行 → 红。
// 修复后：写侧把 legacy 内部群的空 effective space 归一到创建者 default space，补行
//         落在读侧同一个 space → 命中 → 绿。
func Test_Issue557_LegacyGroup_CreatorBackfillSurvivesSpaceScopedRead(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const (
		creator        = "u-creator-557legacy"
		defaultSpaceID = "s-default-557legacy"
	)

	// 创建者用户
	userDB := user.NewDB(ctx)
	require.NoError(t, userDB.Insert(&user.Model{UID: creator, Name: "creator", ShortNo: "sn557legacy"}))

	// legacy 群：space_id == ''
	groupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{
		GroupNo: groupNo, Name: "legacy 群", Creator: creator, Status: 1, Version: 1, SpaceID: "",
	}))
	// 创建者是**内部**成员（is_external=0），非外部成员
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: creator, Role: group.MemberRoleCreator,
		Status: int(common.GroupMemberStatusNormal), Version: 1, Vercode: util.GenerUUID(),
		IsExternal: 0,
	}))

	// 创建者有一个非空 default space（space_member.status=1）——现代客户端据此读 legacy 群。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, ?, ?, NOW(), NOW())",
		defaultSpaceID, creator, 1, 1,
	).Exec()
	require.NoError(t, err)

	svc := NewService(ctx).(*Service)

	// —— 写侧归一化：legacy 内部群的空 effective space 必须归一到创建者非空 default space ——
	resolved, rerr := svc.resolveCreatorBackfillSpaceID(groupNo, creator)
	require.NoError(t, rerr)
	require.Equal(t, defaultSpaceID, resolved,
		"legacy 内部群创建者补行 space 必须归一到其非空 default space，否则读侧 SQL 过滤会丢行")

	// —— 用归一化后的 space 落创建者补行（复用生产写侧函数）——
	shortID := "1489104291682700001"
	channelID := BuildChannelID(groupNo, shortID)
	convSvc := conversation_ext.NewService(ctx)
	require.NoError(t, convSvc.EnsureThreadFollowForCreator(creator, resolved, channelID))

	// —— 读侧 SQL 过滤层（真正丢行处）：以非空 default space 读，必须命中补行 ——
	convDB := conversation_ext.NewDB(ctx)
	rows, err := convDB.ListThreadExts(creator, defaultSpaceID)
	require.NoError(t, err)

	var found bool
	for _, r := range rows {
		if r.TargetID == channelID {
			found = true
			break
		}
	}
	assert.True(t, found,
		"以非空 default space 读时必须查到创建者子区补行（修复前补行落 space_id='' 被 SQL 过滤 → 红灯）")
}
