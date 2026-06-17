package group

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// TestGroupAvatarVersionDBRoundTrip 覆盖群主手动上传头像的落库路径：
// updateAvatar 必须写入版本化对象 path、版本号，并把 is_upload_avatar 置 1
// （后者是阻止旧的自动合成事件覆盖手动头像的关键标志）。QueryWithGroupNo
// 读回这些字段，avatarGet 据此选择 versioned/legacy path。
func TestGroupAvatarVersionDBRoundTrip(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	const groupNo = "avatar_ver_group_1"
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo,
		Name:    "avatar group",
		Creator: "creator_uid_1",
		Status:  1,
	}))

	// 上传前：自动合成允许（is_upload_avatar=0），版本为 0。
	before, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Equal(t, 0, before.IsUploadAvatar)
	require.Equal(t, int64(0), before.AvatarVersion)

	// 群主手动上传：写版本化 path + 版本号 + is_upload_avatar=1。
	const version int64 = 1733300000000000002
	avatarPath := ctx.GetConfig().GetGroupAvatarFilePath(groupNo, version)
	require.NoError(t, g.db.updateAvatar(avatarPath, version, groupNo))

	after, err := g.db.QueryWithGroupNo(groupNo)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, 1, after.IsUploadAvatar)
	require.Equal(t, version, after.AvatarVersion)
	require.Equal(t, avatarPath, after.Avatar)
}
