package category

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/require"
)

const projectGroupsEndpointDefaultLimit = 50

func TestSidebarSectionsListsJoinedProjectsAndOwnCategories(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	spaceID := "sidebar-sections-space"
	seedSpaceAndMember(t, c, spaceID, 0)

	for i := 0; i < 3; i++ {
		categoryID := util.GenerUUID()
		var isDefault any
		if i == 0 {
			isDefault = 1
		}
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, ?)",
			categoryID, spaceID, testutil.UID, "category", i, isDefault,
		).Exec()
		require.NoError(t, err)
	}

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		projectID := util.GenerUUID()
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO octo_project (project_id, space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
			projectID, spaceID, fmt.Sprintf("project-%d", i), testutil.UID, now, now,
		).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) VALUES (?, ?, ?, 0, 1, ?, ?, ?)",
			projectID, testutil.UID, spaceID, testutil.UID, now, now,
		).Exec()
		require.NoError(t, err)
	}

	w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	sections := parseJSONArray(t, w)
	require.Len(t, sections, 5)
	var projectSectionCount int
	require.NoError(t, ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM octo_sidebar_section WHERE uid=? AND space_id=? AND section_type=? AND status=1",
		testutil.UID, spaceID, sidebarSectionTypeProject,
	).LoadOne(&projectSectionCount))
	require.Equal(t, 2, projectSectionCount, "the list path must repair memberships whose post-commit hook did not run")
	for _, section := range sections {
		if section["type"] != sidebarSectionAPITypeProject {
			continue
		}
		project, ok := section["project"].(map[string]any)
		require.True(t, ok)
		require.NotEmpty(t, project["project_id"])
		require.NotEmpty(t, project["project_name"], "every sidebar Project payload pairs project_id with project_name")
	}
}

func TestPinnedSpaceListedProjectBackfillsAndRendersSidebarSection(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const (
		spaceID     = "sidebar-pinned-project-space"
		projectID   = "sidebar-pinned-project"
		projectName = "pinned project"
	)
	seedSpaceAndMember(t, c, spaceID, 0)
	now := time.Now().UTC()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, creator, discoverability, status, created_at, updated_at) VALUES (?, ?, ?, ?, 0, 1, ?, ?)",
		projectID, spaceID, projectName, "another-project-member", now, now,
	).Exec()
	require.NoError(t, err)
	// This user can see a Space-listed Project but has no Project seat. #861
	// permits the pin; the sidebar must materialize the matching entry and keep
	// its group list empty rather than pretending this user joined its groups.
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO octo_project_user_setting (project_id, uid, pinned, pinned_at, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?)",
		projectID, testutil.UID, now, now, now,
	).Exec()
	require.NoError(t, err)

	w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var found map[string]any
	for _, section := range parseJSONArray(t, w) {
		if section["type"] == sidebarSectionAPITypeProject && section["id"] == projectID {
			found = section
			break
		}
	}
	require.NotNil(t, found, "a pinned Space-listed Project must enter Follow even without a Project seat")
	project, ok := found["project"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, projectID, project["project_id"])
	require.Equal(t, projectName, project["project_name"])
	require.Empty(t, project["groups"], "only Project membership grants groups")

	var sections int
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND section_type=? AND ref_id=? AND status=1", testutil.UID, spaceID, sidebarSectionTypeProject, projectID).
		LoadOne(&sections))
	require.Equal(t, 1, sections, "the list backstop must repair a missed pin hook")
}

func TestSidebarSectionsIsUIDRateLimited(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const spaceID = "sidebar-sections-rate-limit-space"
	seedSpaceAndMember(t, c, spaceID, 0)

	for i := 0; i < 70; i++ {
		w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
		if w.Code == http.StatusTooManyRequests {
			require.Equal(t, "uid", w.Header().Get("X-RateLimit-Scope"))
			return
		}
		require.Equalf(t, http.StatusOK, w.Code, "request %d: %s", i, w.Body.String())
	}
	t.Fatal("expected the authenticated sidebar section route to consume the shared UID bucket")
}

func TestSidebarSectionsOrderInterleavesTypesAndOldCategoriesKeepRelativeOrder(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const spaceID = "sidebar-interleave-space"
	seedSpaceAndMember(t, c, spaceID, 0)
	categoryA := seedSidebarCategory(t, ctx, spaceID, "category-a", 0, 1)
	categoryB := seedSidebarCategory(t, ctx, spaceID, "category-b", 1, nil)
	projectID := seedSidebarProjectMembership(t, ctx, spaceID, "sidebar-interleave-project", "project")

	// The first read creates the missing Project section from the active seat.
	w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Len(t, parseJSONArray(t, w), 3)

	w = doRequest(t, s.GetRoute(), http.MethodPut, "/v1/spaces/"+spaceID+"/sidebar-sections/sort", map[string]any{
		"items": []map[string]string{
			{"type": sidebarSectionAPITypeCategory, "id": categoryA},
			{"type": sidebarSectionAPITypeProject, "id": projectID},
			{"type": sidebarSectionAPITypeCategory, "id": categoryB},
		},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	sections := parseJSONArray(t, w)
	require.Len(t, sections, 3)
	require.Equal(t, sidebarSectionAPITypeCategory, sections[0]["type"])
	require.Equal(t, categoryA, sections[0]["id"])
	require.Equal(t, sidebarSectionAPITypeProject, sections[1]["type"])
	require.Equal(t, projectID, sections[1]["id"])
	require.Equal(t, sidebarSectionAPITypeCategory, sections[2]["type"])
	require.Equal(t, categoryB, sections[2]["id"])

	// The legacy category-only view intentionally filters Project entries, but
	// its remaining categories must preserve their relative unified order.
	w = doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/categories", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	categories := parseJSONArray(t, w)
	require.Len(t, categories, 2)
	require.Equal(t, categoryA, categories[0]["category_id"])
	require.Equal(t, categoryB, categories[1]["category_id"])

	// A legacy category-only client cannot name the Project entry, but sorting
	// the categories must still preserve the slots occupied by Project sections.
	// Even a no-op category order used to collapse categoryB onto the Project's
	// sort value and move the Project behind categoryB through the id tie-break.
	w = doRequest(t, s.GetRoute(), http.MethodPut, "/v1/spaces/"+spaceID+"/categories/sort", map[string]any{
		"category_ids": []string{categoryA, categoryB},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	sections = parseJSONArray(t, w)
	require.Len(t, sections, 3)
	require.Equal(t, categoryA, sections[0]["id"])
	require.Equal(t, projectID, sections[1]["id"], "legacy category sort must preserve the Project slot")
	require.Equal(t, categoryB, sections[2]["id"])
}

func TestOldCategoriesSortWritesSidebarSectionTable(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const spaceID = "sidebar-old-sort-space"
	seedSpaceAndMember(t, c, spaceID, 0)
	categoryA := seedSidebarCategory(t, ctx, spaceID, "category-a", 37, nil)
	categoryB := seedSidebarCategory(t, ctx, spaceID, "category-b", 12, nil)

	w := doRequest(t, s.GetRoute(), http.MethodPut, "/v1/spaces/"+spaceID+"/categories/sort", map[string]any{
		"category_ids": []string{categoryB, categoryA},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var rows []*struct {
		RefID string `db:"ref_id"`
		Sort  int    `db:"sort"`
	}
	_, err := ctx.DB().Select("ref_id", "sort").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND section_type=?", testutil.UID, spaceID, sidebarSectionTypeCategory).
		OrderAsc("sort").Load(&rows)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, categoryB, rows[0].RefID)
	require.Equal(t, 0, rows[0].Sort)
	require.Equal(t, categoryA, rows[1].RefID)
	require.Equal(t, 1, rows[1].Sort)

	// Old group_category.sort was only migration input and must not silently
	// become a second writer after the authority handover.
	var legacySort int
	require.NoError(t, ctx.DB().Select("sort").From("group_category").Where("category_id=?", categoryA).LoadOne(&legacySort))
	require.Equal(t, 37, legacySort)
}

func TestDeletingCategoryHidesSidebarSection(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const spaceID = "sidebar-delete-category-space"
	seedSpaceAndMember(t, c, spaceID, 0)
	categoryID := seedSidebarCategory(t, ctx, spaceID, "category-to-delete", 0, nil)

	// Materialize the legacy category's section first, as an existing deployment
	// would have done through the migration or the list-path backstop.
	w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doRequest(t, s.GetRoute(), http.MethodDelete, "/v1/spaces/"+spaceID+"/categories/"+categoryID, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var status int
	require.NoError(t, ctx.DB().Select("status").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND section_type=? AND ref_id=?", testutil.UID, spaceID, sidebarSectionTypeCategory, categoryID).
		LoadOne(&status))
	require.Equal(t, 2, status)

	w = doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	for _, section := range parseJSONArray(t, w) {
		require.NotEqual(t, categoryID, section["id"])
	}
}

func TestSidebarSectionProjectContentMatchesProjectGroupsEndpoint(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const spaceID = "sidebar-project-content-space"
	seedSpaceAndMember(t, c, spaceID, 0)
	projectID := seedSidebarProjectMembership(t, ctx, spaceID, "sidebar-project-content", "project")
	for i := 0; i < projectGroupsEndpointDefaultLimit+1; i++ {
		groupNo := fmt.Sprintf("sidebar-project-content-group-%d", i)
		seedGroup(t, c, groupNo, spaceID)
		_, err := ctx.DB().UpdateBySql("UPDATE `group` SET project_id=? WHERE group_no=?", projectID, groupNo).Exec()
		require.NoError(t, err)
	}

	w := doRequest(t, s.GetRoute(), http.MethodGet, "/v1/spaces/"+spaceID+"/sidebar-sections", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var projectGroupsFromSection any
	for _, section := range parseJSONArray(t, w) {
		if section["type"] == sidebarSectionAPITypeProject && section["id"] == projectID {
			projectGroupsFromSection = section["project"].(map[string]any)["groups"]
			break
		}
	}
	require.NotNil(t, projectGroupsFromSection, "the active Project membership must render one Project section")

	w = doRequest(t, s.GetRoute(), http.MethodGet, "/v1/projects/"+projectID+"/groups", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	endpointGroups := parseJSONArray(t, w)
	require.Len(t, endpointGroups, projectGroupsEndpointDefaultLimit, "the comparison must exercise the endpoint default page limit")
	sectionJSON, err := json.Marshal(projectGroupsFromSection)
	require.NoError(t, err)
	endpointJSON, err := json.Marshal(endpointGroups)
	require.NoError(t, err)
	require.JSONEq(t, string(endpointJSON), string(sectionJSON))
}

func TestMoveGroupToCategoryRejectsProjectGroup(t *testing.T) {
	s, ctx := newCategoryTestServer()
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	resetUIDRateLimit(t, ctx)

	c := New(ctx)
	const (
		spaceID = "sidebar-project-group-move-space"
		groupNo = "sidebar-project-group-move-group"
	)
	seedSpaceAndMember(t, c, spaceID, 0)
	categoryA := seedSidebarCategory(t, ctx, spaceID, "category-a", 0, 1)
	categoryB := seedSidebarCategory(t, ctx, spaceID, "category-b", 1, nil)
	projectID := seedSidebarProjectMembership(t, ctx, spaceID, "sidebar-project-group-move", "project")
	seedGroup(t, c, groupNo, spaceID)
	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET project_id=? WHERE group_no=?", projectID, groupNo).Exec()
	require.NoError(t, err)
	require.NoError(t, c.db.insertGroupSettingForCategory(groupNo, testutil.UID, &categoryA, 0, 1))

	w := doRequest(t, s.GetRoute(), http.MethodPut, "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": categoryB})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	assertCategoryErrorCode(t, w, errcode.ErrCategoryProjectGroupCannotCategorize.ID)

	setting, err := c.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	require.NoError(t, err)
	require.NotNil(t, setting)
	require.NotNil(t, setting.CategoryID)
	require.Equal(t, categoryA, *setting.CategoryID, "rejection must not modify the existing manual assignment")

	// Historical bad rows (including ones created before the mutual-exclusion
	// guard existed) must remain repairable through the public API. Clearing a
	// category does not categorize the Project group and is therefore allowed.
	w = doRequest(t, s.GetRoute(), http.MethodPut, "/v1/groups/"+groupNo+"/category", map[string]string{"category_id": ""})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	setting, err = c.db.queryGroupSettingForCategory(groupNo, testutil.UID)
	require.NoError(t, err)
	require.NotNil(t, setting)
	require.Nil(t, setting.CategoryID)
}

func TestSidebarSectionMigrationBackfillsOrderProjectsAndProjectGroupCleanup(t *testing.T) {
	_, ctx, _ := newToctouTestServer(t)
	_, err := ctx.DB().UpdateBySql("DROP TABLE octo_sidebar_section").Exec()
	require.NoError(t, err)

	const (
		uid       = "migration-sidebar-user"
		spaceID   = "migration-sidebar-space"
		categoryA = "migration-sidebar-category-a"
		categoryB = "migration-sidebar-category-b"
		projectA  = "migration-sidebar-project-a"
		projectB  = "migration-sidebar-project-b"
		groupA    = "migration-sidebar-project-group"
		groupB    = "migration-sidebar-direct-group"
	)
	now := time.Now().UTC()
	for _, category := range []struct {
		id   string
		sort int
	}{{categoryA, 4}, {categoryB, 9}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, NULL)",
			category.id, spaceID, uid, category.id, category.sort,
		).Exec()
		require.NoError(t, err)
	}
	for _, projectID := range []string{projectA, projectB} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO octo_project (project_id, space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
			projectID, spaceID, projectID, uid, now, now,
		).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) VALUES (?, ?, ?, 0, 1, ?, ?, ?)",
			projectID, uid, spaceID, uid, now, now,
		).Exec()
		require.NoError(t, err)
	}
	for _, group := range []struct {
		groupNo   string
		projectID string
		category  string
	}{{groupA, projectA, categoryA}, {groupB, "", categoryB}} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) VALUES (?, ?, ?, 1, ?, ?)",
			group.groupNo, group.groupNo, uid, spaceID, group.projectID,
		).Exec()
		require.NoError(t, err)
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO group_setting (group_no, uid, category_id, category_sort) VALUES (?, ?, ?, 7)",
			group.groupNo, uid, group.category,
		).Exec()
		require.NoError(t, err)
	}

	migrationFile, err := os.Open("sql/20260909000001_sidebar_project_sections.sql")
	require.NoError(t, err)
	defer migrationFile.Close()
	migration, err := migrate.ParseMigration("20260909000001_sidebar_project_sections.sql", migrationFile)
	require.NoError(t, err)
	for _, statement := range migration.Up {
		_, err := ctx.DB().DB.Exec(statement)
		require.NoError(t, err, "execute migration statement: %s", statement)
	}

	var categoryRows []*sidebarSectionOrderRow
	_, err = ctx.DB().Select("section_type", "ref_id", "sort").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND section_type=?", uid, spaceID, sidebarSectionTypeCategory).
		OrderAsc("sort").Load(&categoryRows)
	require.NoError(t, err)
	require.Len(t, categoryRows, 2)
	require.Equal(t, categoryA, categoryRows[0].RefID)
	require.Equal(t, categoryB, categoryRows[1].RefID)

	var projectRows int
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("octo_sidebar_section").
		Where("uid=? AND space_id=? AND section_type=?", uid, spaceID, sidebarSectionTypeProject).
		LoadOne(&projectRows))
	require.Equal(t, 2, projectRows)
	assertGroupCategoryAssignment(t, ctx, groupA, uid, "", 0)
	assertGroupCategoryAssignment(t, ctx, groupB, uid, categoryB, 7)

	// INSERT IGNORE plus the typed unique key make the backfill itself safe to
	// retry after an interrupted rollout. CREATE TABLE is intentionally not rerun.
	for _, statement := range migration.Up[1:] {
		_, err := ctx.DB().DB.Exec(statement)
		require.NoError(t, err, "re-execute idempotent backfill statement: %s", statement)
	}
	var total int
	require.NoError(t, ctx.DB().Select("COUNT(*)").From("octo_sidebar_section").
		Where("uid=? AND space_id=?", uid, spaceID).LoadOne(&total))
	require.Equal(t, 4, total)
}

func seedSidebarCategory(t *testing.T, ctx *config.Context, spaceID, name string, sort int, isDefault any) string {
	t.Helper()
	categoryID := util.GenerUUID()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status, is_default) VALUES (?, ?, ?, ?, ?, 1, ?)",
		categoryID, spaceID, testutil.UID, name, sort, isDefault,
	).Exec()
	require.NoError(t, err)
	return categoryID
}

func seedSidebarProjectMembership(t *testing.T, ctx *config.Context, spaceID, projectID, name string) string {
	t.Helper()
	now := time.Now().UTC()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?)",
		projectID, spaceID, name, testutil.UID, now, now,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, invite_uid, created_at, updated_at) VALUES (?, ?, ?, 0, 1, ?, ?, ?)",
		projectID, testutil.UID, spaceID, testutil.UID, now, now,
	).Exec()
	require.NoError(t, err)
	return projectID
}

func assertGroupCategoryAssignment(t *testing.T, ctx *config.Context, groupNo, uid, wantCategoryID string, wantSort int) {
	t.Helper()
	var row struct {
		CategoryID   *string `db:"category_id"`
		CategorySort int     `db:"category_sort"`
	}
	require.NoError(t, ctx.DB().Select("category_id", "category_sort").From("group_setting").
		Where("group_no=? AND uid=?", groupNo, uid).LoadOne(&row))
	if wantCategoryID == "" {
		require.Nil(t, row.CategoryID)
	} else {
		require.NotNil(t, row.CategoryID)
		require.Equal(t, wantCategoryID, *row.CategoryID)
	}
	require.Equal(t, wantSort, row.CategorySort)
}
