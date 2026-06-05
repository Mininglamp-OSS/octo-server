package user

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

// TestUserAvatarVersionDBRoundTrip 覆盖 uploadAvatar 的落库路径：
// UpdateAvatarUploadStatus 必须把 is_upload_avatar 翻成 1 并写入服务端版本号，
// QueryByUID 能读回该版本，进而选出版本化对象 path。
// 端到端的 TestUploadAvatar 因 issue #17 被 t.Skip，这里补 DB 层集成覆盖。
func TestUserAvatarVersionDBRoundTrip(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	u := New(ctx)

	const uid = "avatar_ver_uid_1"
	require.NoError(t, u.db.Insert(&Model{
		UID:      uid,
		Name:     "avatar tester",
		Username: "avatar_ver_user_1",
		ShortNo:  "avatar_ver_sn_1",
		Status:   1,
	}))

	partition := ctx.GetConfig().Avatar.Partition

	// 上传前：旧用户落回 legacy 稳定 path（不含版本段）。
	before, err := u.db.QueryByUID(uid)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Equal(t, 0, before.IsUploadAvatar)
	require.Equal(t, int64(0), before.AvatarVersion)
	legacyPath := userAvatarFilePath(uid, partition, before.AvatarVersion)
	require.True(t, strings.HasSuffix(legacyPath, fmt.Sprintf("/%s.png", uid)),
		"version=0 must fall back to legacy path, got %q", legacyPath)

	// 上传：标记已上传并写入版本。
	const version int64 = 1733300000000000001
	require.NoError(t, u.db.UpdateAvatarUploadStatus(uid, version))

	after, err := u.db.QueryByUID(uid)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, 1, after.IsUploadAvatar)
	require.Equal(t, version, after.AvatarVersion)

	// 上传后：读回的版本选出版本化 path（含 /{version}.png）。
	versionedPath := userAvatarFilePath(uid, partition, after.AvatarVersion)
	require.True(t, strings.HasSuffix(versionedPath, fmt.Sprintf("/%d.png", version)),
		"version>0 must select versioned path, got %q", versionedPath)
	require.NotEqual(t, legacyPath, versionedPath)
}
