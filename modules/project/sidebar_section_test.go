package project

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
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
