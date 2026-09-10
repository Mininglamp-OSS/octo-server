package project

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectCreateProvisionsSidebarSection(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-owner")
	seedSpaceMember(t, spaceA, "sidebar-owner", 0, 1)

	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar create")
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-owner", 1)
}

func TestProjectAdmitProvisionsSidebarSection(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-owner")
	seedSpaceMember(t, spaceA, "sidebar-owner", 0, 1)
	seedUser(t, "sidebar-member")
	seedSpaceMember(t, spaceA, "sidebar-member", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar admit")

	w := doJSON(t, srv, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{"sidebar-member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-member", 1)

	// The admission's section is personal; the creator's row must not have been
	// duplicated by a later member write.
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-owner", 1)
}

func TestPinningSpaceListedProjectProvisionsSidebarSection(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-pin-owner")
	pinnerToken := seedUser(t, "sidebar-pinner")
	seedSpaceMember(t, spaceA, "sidebar-pin-owner", 0, 1)
	seedSpaceMember(t, spaceA, "sidebar-pinner", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar pin")

	var seats int
	require.NoError(t, testCtx.DB().Select("COUNT(*)").From("octo_project_member").
		Where("project_id=? AND uid=? AND status=1 AND removing=0", created.ProjectID, "sidebar-pinner").
		LoadOne(&seats))
	require.Zero(t, seats, "the regression must cover #861's Space-listed non-member pin path")
	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, pinnerToken, true).Code)
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-pinner", 1)
}

func TestUnpinningProjectRemovesItFromFollowAndRepinningRestoresIt(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-unpin-owner")
	seedSpaceMember(t, spaceA, "sidebar-unpin-owner", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar unpin")

	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, ownerToken, true).Code)
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, true)

	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, ownerToken, false).Code)
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, false)
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-unpin-owner", 0)

	// A second read exercises the membership repair path. The explicit unpin is
	// a personal opt-out and must not be undone merely because the user still has
	// an active Project seat.
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, false)

	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, ownerToken, true).Code)
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, true)
	assertProjectSidebarSection(t, created.ProjectID, "sidebar-unpin-owner", 1)
}

func TestUnpinningNeverPinnedMemberStaysOutOfFollow(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-never-pinned-owner")
	seedSpaceMember(t, spaceA, "sidebar-never-pinned-owner", 0, 1)
	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar never pinned")

	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, true)
	require.Equal(t, http.StatusOK, setPinned(t, srv, created.ProjectID, ownerToken, false).Code)

	// Both reads exercise the repair path. The first explicit false write must
	// survive even though this member never had a pinned=true setting row.
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, false)
	assertProjectInFollow(t, srv, ownerToken, created.ProjectID, false)
}

func TestProjectSidebarProvisionFailureDoesNotRollbackProject(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	ownerToken := seedUser(t, "sidebar-fail-owner")
	seedSpaceMember(t, spaceA, "sidebar-fail-owner", 0, 1)

	original := projectSidebarSectionProvisioner()
	RegisterSidebarSectionProvisioner(func(*config.Context, string, string, string) error {
		return errors.New("sidebar section storage unavailable")
	})
	t.Cleanup(func() { RegisterSidebarSectionProvisioner(original) })

	created := createProjectVia(t, srv, spaceA, ownerToken, "sidebar hook failure")
	var projects int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_project WHERE project_id=? AND status=?", created.ProjectID, StatusNormal,
	).LoadOne(&projects))
	assert.Equal(t, 1, projects, "best-effort sidebar provisioning must not roll back a committed Project")
}

func assertProjectSidebarSection(t *testing.T, projectID, uid string, want int) {
	t.Helper()
	var count int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_sidebar_section WHERE uid=? AND space_id=? AND section_type=2 AND ref_id=? AND status=1",
		uid, spaceA, projectID,
	).LoadOne(&count))
	assert.Equal(t, want, count)
}

func assertProjectInFollow(t *testing.T, srv *server.Server, token, projectID string, want bool) {
	t.Helper()
	w := doJSON(t, srv, http.MethodGet, "/v1/spaces/"+spaceA+"/sidebar-sections", token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var sections []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sections), "body: %s", w.Body.String())
	for _, section := range sections {
		if section.Type == "project" && section.ID == projectID {
			assert.True(t, want, "Project %s unexpectedly remains in Follow", projectID)
			return
		}
	}
	assert.False(t, want, "Project %s is missing from Follow", projectID)
}
