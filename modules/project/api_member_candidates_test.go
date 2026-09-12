package project

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memberCandidateTestResp struct {
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

func seedProjectCandidateUser(t *testing.T, uid, name string) {
	t.Helper()
	seedUser(t, uid)
	_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET name = ? WHERE uid = ?", name, uid).Exec()
	require.NoError(t, err)
	seedSpaceMember(t, spaceA, uid, 0, 1)
}

func TestProjectMemberCandidatesClassifyCurrentAlreadyAndInvitable(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv, "already", "removed")

	seedProjectCandidateUser(t, "invitable", "Invitable Person")

	// A historical Project seat remains in the table after removal, but the
	// candidate projection must classify it as invitable because only active,
	// non-removing seats are already members.
	w := doJSON(t, srv, http.MethodPost,
		"/v1/projects/"+created.ProjectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{"removed"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates", ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "4", w.Header().Get("X-Total-Count"))

	var rows []memberCandidateTestResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows), "body: %s", w.Body.String())
	require.Len(t, rows, 4)
	assert.Equal(t, "owner1", rows[0].UID, "the current user must be first")
	assert.Equal(t, "current_user", rows[0].Status)

	statuses := make(map[string]string, len(rows))
	for _, row := range rows {
		statuses[row.UID] = row.Status
	}
	assert.Equal(t, "already_member", statuses["already"])
	assert.Equal(t, "invitable", statuses["removed"])
	assert.Equal(t, "invitable", statuses["invitable"])
}

func TestProjectMemberCandidatesEscapeLiteralNameAndUseProjectPaging(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, _, created := projectWithMembers(t, srv)
	seedProjectCandidateUser(t, "literal-name", "Literal_%!")
	seedProjectCandidateUser(t, "other-one", "Other One")
	seedProjectCandidateUser(t, "other-two", "Other Two")

	search := url.Values{"keyword": []string{"Literal_%!"}}
	w := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates?"+search.Encode(), ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "1", w.Header().Get("X-Total-Count"))
	var matched []memberCandidateTestResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &matched), "body: %s", w.Body.String())
	require.Len(t, matched, 1)
	assert.Equal(t, "literal-name", matched[0].UID)
	assert.Equal(t, "Literal_%!", matched[0].Name)

	page := url.Values{"page": []string{"2"}, "limit": []string{"2"}}
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates?"+page.Encode(), ownerToken, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "4", w.Header().Get("X-Total-Count"))
	var secondPage []memberCandidateTestResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &secondPage), "body: %s", w.Body.String())
	assert.Len(t, secondPage, 2)
}

func TestProjectMemberCandidatesRequireAdminAndSameSpace(t *testing.T) {
	srv, _ := setup(t)
	ownerToken, tokens, created := projectWithMembers(t, srv, "ordinary")

	w := doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates", tokens["ordinary"], nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.permission_denied")

	seedSpace(t, spaceB, 1)
	foreignToken := seedUser(t, "foreign-caller")
	seedSpaceMember(t, spaceB, "foreign-caller", 0, 1)
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates", foreignToken, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.not_found")

	_, err := testCtx.DB().UpdateBySql("UPDATE `user` SET status = 0 WHERE uid = ?", "owner1").Exec()
	require.NoError(t, err)
	w = doJSON(t, srv, http.MethodGet,
		"/v1/projects/"+created.ProjectID+"/member-candidates", ownerToken, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.not_found")
}
