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
// drift of a version token (an accidental revert group-name-v3 -> v2, or a forgotten
// bump on the next visual change) would silently change the served image's cache
// identity with no other test failing, and clients holding the old ETag would 304
// onto a stale image. That is exactly the #486 staleness root cause (precedent #349).
//
// Existing endpoint tests assert the ETag is present / weak / changes on rename, but
// none pin the version literal. This recomputes the expected ETag with the expected
// version string and asserts the endpoint emits exactly it, so any token drift fails
// loudly here. GroupText is the same helper the handler uses, so a legitimate change
// to the first-N-runes text rule moves test and handler in lockstep — only the version
// segment is pinned.
func TestGroupAvatarGetPinsRenderVersion(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	g := New(ctx)

	// Named, renderable group, no custom color -> name render mode, default colorTag "seed".
	const namedNo = "avatar_pin_named_1"
	require.NoError(t, g.db.Insert(&Model{GroupNo: namedNo, Name: "后端架构讨论", Creator: "c1", Status: 1}))
	named := doAvatarGet(t, s.GetRoute(), namedNo, "")
	require.Equal(t, http.StatusOK, named.Code)
	wantName := avatarrender.ETag("group-name-v3", namedNo, "seed", avatarrender.GroupText("后端架构讨论"))
	require.Equal(t, wantName, named.Header().Get("ETag"),
		"group name-avatar endpoint must wire render version group-name-v3 (keep in sync with the rendered bytes)")

	// Empty name -> icon fallback mode -> "group-icon-v3".
	const iconNo = "avatar_pin_icon_1"
	require.NoError(t, g.db.Insert(&Model{GroupNo: iconNo, Name: "", Creator: "c1", Status: 1}))
	icon := doAvatarGet(t, s.GetRoute(), iconNo, "")
	require.Equal(t, http.StatusOK, icon.Code)
	wantIcon := avatarrender.ETag("group-icon-v3", iconNo, "seed")
	require.Equal(t, wantIcon, icon.Header().Get("ETag"),
		"group icon-avatar endpoint must wire render version group-icon-v3")
}
