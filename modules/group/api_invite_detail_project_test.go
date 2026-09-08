package group

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupInviteDetailNeverLeaksProjectID keeps project_id off the one group
// surface that answers UNAUTHENTICATED callers.
//
// #855 put project_id on GroupResp and the group detail, and PR-2 of
// project-p2-product-surfaces puts it on the sidebar. Every one of those is behind
// AuthMiddleware and answers a member of the group, who by I2 is already an active
// member of the project — so the field tells them nothing their own project roster
// does not.
//
// GET /v1/group/invite/detail is the exception: it deliberately carries no
// AuthMiddleware (public H5 landing page, optional token per YUJ-39), so anyone
// holding an invite code reaches it. project_id there would let an anonymous caller
// learn that a project exists and its id, from a code that says nothing about it.
//
// It is safe today only because the handler builds its response as a hand-written
// gin.H rather than through GroupResp. That is an accident of how it was written,
// not a guard — so this test is the guard. The way it breaks is a tidy-up:
// "why does this endpoint build its own map, let us reuse GroupResp".
func TestGroupInviteDetailNeverLeaksProjectID(t *testing.T) {
	s, ctx := newTestServer(t)
	f := New(ctx)
	require.NoError(t, testutil.CleanAllTables(ctx))

	const spaceID = "space-invite-project"
	_, err := ctx.DB().InsertInto("space").
		Columns("space_id", "name", "creator", "status").
		Values(spaceID, "项目空间", testutil.UID, 1).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("space_member").
		Columns("space_id", "uid", "role", "status").
		Values(spaceID, testutil.UID, 0, 1).Exec()
	require.NoError(t, err)

	// A group that really does belong to a project — the whole point is that the
	// field EXISTS on the row and still does not reach this response.
	const groupNo = "g-invite-detail-project"
	const projectID = "p-invite-detail"
	require.NoError(t, f.db.Insert(&Model{
		GroupNo:       groupNo,
		Name:          "项目群",
		Creator:       testutil.UID,
		Status:        1,
		SpaceID:       spaceID,
		ProjectID:     projectID,
		AllowExternal: 1,
	}))
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: MemberRoleCreator, Version: 1,
	}))

	code := "invite-detail-project-code"
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		fmt.Sprintf("%s%s", common.QRCodeCachePrefix, code),
		util.ToJson(common.NewQRCodeModel(common.QRCodeTypeGroup, map[string]interface{}{
			"group_no":  groupNo,
			"generator": testutil.UID,
		})),
		time.Hour,
	))

	// Anonymous: no token header at all, which is the population this guards.
	req := newInviteRequest(t, "/v1/group/invite/detail?code="+code)
	w := httptest.NewRecorder()
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.NotContains(t, resp, "project_id",
		"the public invite preview must not disclose project_id: it has no "+
			"AuthMiddleware, so an anonymous holder of an invite code would learn "+
			"that a project exists and what its id is")
	assert.NotContains(t, w.Body.String(), projectID,
		"and not under any other key either — asserted on the raw body so a rename "+
			"of the field cannot slip the value through")
}
