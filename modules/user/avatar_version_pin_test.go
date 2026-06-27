package user

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/stretchr/testify/require"
)

// TestUserAvatarGetPinsRenderVersion pins the render-mode version token (name-v5) the
// personal default-avatar endpoint wires into the ETag. Same rationale as the group
// pin: the ETag is a CRC32 over content factors (mode-version + uid + text), NOT the
// PNG bytes, so a silent version drift (accidental revert name-v5 -> name-v4, or a
// forgotten bump on the next visual change) would change the served image's cache
// identity with no other test catching it — clients on the old ETag would 304 onto a
// stale image (#486 root cause). IndividualText is the same helper the handler uses,
// so only the version segment is pinned here. ascii-v1 is intentionally not pinned: it
// renders via the unchanged generateDefaultAvatar path and was correctly left un-bumped.
func TestUserAvatarGetPinsRenderVersion(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	ctx.GetConfig().Avatar.Default = ""
	ctx.GetConfig().Avatar.DefaultBaseURL = "" // force the local server-side render path

	u := New(ctx)
	const uid = "avatar_pin_name_1"
	require.NoError(t, u.db.Insert(&Model{UID: uid, Name: "张三丰", Username: uid, ShortNo: "avpin001", Status: 1}))

	w := getAvatarForTest(t, s.GetRoute(), uid) // asserts 200 + image/png
	text := avatarrender.IndividualText("张三丰")
	require.True(t, avatarrender.Renderable(text), "precondition: nickname must be renderable to hit name-v5 mode")
	want := avatarETag("name-v5", uid, text)
	require.Equal(t, want, w.Header().Get("ETag"),
		"personal name-avatar endpoint must wire render version name-v5 (keep in sync with the rendered bytes)")
}
