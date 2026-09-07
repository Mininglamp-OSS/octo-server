package group

// P1 round-1 review: an admission refusal reached the wire as a 500.
//
// admitOrRestoreMembersTx returns ErrAdmissionRefused when the composite gate
// says no. Two converged paths mapped it correctly (scan-join, blacklist); three
// did not — group create, invite/sure, and the batch member add all let it fall
// into the generic ErrGroupStoreFailed arm, which is registered Internal, so the
// renderer HIDES the message and the client is told the server broke.
//
// That is wrong in three separate ways and each one costs something real:
//
//   - it is a CALLER error (this uid may not join this project's group), and a
//     client cannot distinguish "retry later" from "this will never work";
//   - Internal = true means 5xx, which pages whoever owns the alert on the most
//     ordinary refusal the feature has;
//   - it makes the one code P1 registered for exactly this refusal
//     (ErrGroupProjectMemberRequired) unreachable from the paths that matter,
//     so the localized copy is never shown.
//
// Asserted at the HTTP boundary rather than as a source guard: what is under
// test is the status and the code id a client actually receives.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreatingAProjectGroupWithANonMemberIsA400NotA500.
//
// The creator holds a Space seat and names a project they are not a member of.
// The gate refuses them inside the create transaction — correctly — and the
// question this pins is only what the client is told about it.
func TestCreatingAProjectGroupWithANonMemberIsA400NotA500(t *testing.T) {
	s, ctx := newTestServer(t)
	// The v2 envelope only appears once a renderer is wired; without it
	// ResponseErrorL falls back to the legacy {msg,status} pair and the code id —
	// the thing under test — is not on the wire at all.
	wireI18nRendererForGroupTest(s)
	// No f.Route: newTestServer already mounts the module's routes, and
	// registering them twice panics gin.
	f := New(ctx)

	// The create path refuses a project_id outright while the feature switch is
	// off, so the switch has to be on for the request to reach the gate this test
	// is about.
	t.Setenv("OCTO_PROJECT_CREATE_ENABLED", "true")

	spaceID := "space-adm-refusal-001"
	projectID := util.GenerUUID()

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "creator", ShortNo: "adm_u10000"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: "20009", Name: "member", ShortNo: "adm_u20009"}))
	seedSpaceSeat(t, ctx, spaceID, testutil.UID)
	seedSpaceSeat(t, ctx, spaceID, "20009")
	seedProject(t, ctx, projectID, spaceID)
	// Deliberately NO octo_project_member row for either uid.

	w := httptest.NewRecorder()
	req, err := http.NewRequest("POST", "/v1/group/create", bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
		"name":       "项目群",
		"members":    []string{"20009"},
		"space_id":   spaceID,
		"project_id": projectID,
	}))))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	body := w.Body.Bytes()
	env := decodeEnvelope(t, body)
	assert.Equal(t, "err.server.group.project_member_required", env.Error.Code,
		"an admission refusal must surface as the code registered for it, not as the generic "+
			"store-failed code: body=%s", body)

	// The envelope's own http_status is what says 4xx vs 5xx; the WIRE status stays
	// pinned at 400 by ResponseErrorL (D14 compatibility), so asserting w.Code
	// alone could not tell the two codes apart.
	assert.Less(t, env.Error.HTTPStatus, 500,
		"a refusal is a caller error; Internal = true would hide the message and page "+
			"on-call for an ordinary 'you are not in this project': body=%s", body)

	// And nothing may have been written. A correct status code on top of a
	// committed group would satisfy every assertion above and still break I2 —
	// which is why the I2 suite reads rows rather than codes.
	assert.Empty(t, groupNosInSpace(t, ctx, spaceID),
		"a refused create must leave no group behind")
}

// groupNosInSpace lists the groups that ended up in a Space, so a refusal can be
// checked for having written nothing.
func groupNosInSpace(t *testing.T, ctx *config.Context, spaceID string) []string {
	t.Helper()
	var nos []string
	_, err := ctx.DB().SelectBySql(
		"SELECT group_no FROM `group` WHERE space_id = ?", spaceID).Load(&nos)
	require.NoError(t, err)
	return nos
}
