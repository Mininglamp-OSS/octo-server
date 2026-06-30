package group

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/avatarrender"
	"github.com/stretchr/testify/require"
)

// TestGroupAvatarGetPinsRenderVersion pins the render-mode version tokens the group
// default-avatar endpoint wires into the ETag. The ETag is a CRC32 over the content
// *factors* (mode-version + group_no + color + text), NOT the PNG bytes — so a future
// drift of a version token (an accidental revert group-name-v4 -> v3, or a forgotten
// bump on the next visual change) would silently change the served image's cache
// identity with no other test failing, and clients holding the old ETag would 304
// onto a stale image. That is exactly the #486 staleness root cause (precedent #349).
//
// Existing endpoint tests assert the ETag is present / weak / changes on rename, but
// none pin the version literal. This recomputes the expected ETag with the expected
// version string and asserts the endpoint emits exactly it, so any token drift fails
// loudly here. GroupText is the same helper the handler uses for custom text, so a
// legitimate change to the normalize rule moves test and handler in lockstep — only
// the version segment is pinned.
func TestGroupAvatarGetPinsRenderVersion(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	// This fixture uses custom avatar_text -> name render mode, default colorTag "seed".
	// (Legacy is_named=1 groups also render group-name text; new groups fall back to icon.
	//  This test pins the version segment of the text-render mode, regardless of text source.)
	const namedNo = "avatar_pin_named_1"
	require.NoError(t, g.db.Insert(&Model{GroupNo: namedNo, Name: "后端架构讨论", Creator: "c1", Status: 1, AvatarText: "研发"}))
	named := doAvatarGet(t, s.GetRoute(), namedNo, "")
	require.Equal(t, http.StatusOK, named.Code)
	wantName := avatarrender.ETag("group-name-v4", namedNo, "seed", avatarrender.GroupText("研发"))
	require.Equal(t, wantName, named.Header().Get("ETag"),
		"group text-avatar endpoint must wire render version group-name-v4 (keep in sync with the rendered bytes)")

	// Empty name -> icon fallback mode -> "group-icon-v3".
	const iconNo = "avatar_pin_icon_1"
	require.NoError(t, g.db.Insert(&Model{GroupNo: iconNo, Name: "", Creator: "c1", Status: 1}))
	icon := doAvatarGet(t, s.GetRoute(), iconNo, "")
	require.Equal(t, http.StatusOK, icon.Code)
	wantIcon := avatarrender.ETag("group-icon-v3", iconNo, "seed")
	require.Equal(t, wantIcon, icon.Header().Get("ETag"),
		"group icon-avatar endpoint must wire render version group-icon-v3")
}
