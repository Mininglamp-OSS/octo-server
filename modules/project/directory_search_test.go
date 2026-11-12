package project

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectDirectorySearchDescriptionPaginationAndIsolation(t *testing.T) {
	_, p := setup(t)
	uid := "directory-search-owner"
	token := seedUser(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	seedSpaceMember(t, spaceB, uid, 0, 1)

	create := func(space, name, description string) string {
		t.Helper()
		row, err := p.createProject(createInput{SpaceID: space, Creator: uid, Name: name, Description: description})
		require.NoError(t, err)
		return row.ProjectID
	}
	nameOnly := create(spaceA, "needle title", "unrelated")
	descriptionOnly := create(spaceA, "description only", "contains needle here")
	both := create(spaceA, "needle both", "needle")
	create(spaceA, "no match", "unrelated")
	create(spaceB, "other space", "needle")
	archived := create(spaceA, "archived", "needle")
	_, err := testCtx.DB().UpdateBySql("UPDATE octo_project SET archived_at = UTC_TIMESTAMP(3) WHERE project_id = ?", archived).Exec()
	require.NoError(t, err)
	inactive := create(spaceA, "inactive membership", "needle")
	_, err = testCtx.DB().UpdateBySql("UPDATE octo_project_member SET status = 0 WHERE project_id = ? AND uid = ?", inactive, uid).Exec()
	require.NoError(t, err)
	removing := create(spaceA, "removing membership", "needle")
	_, err = testCtx.DB().UpdateBySql("UPDATE octo_project_member SET removing = 1 WHERE project_id = ? AND uid = ?", removing, uid).Exec()
	require.NoError(t, err)

	r := mountProject(t, p)
	read := func(query, total string) []string {
		t.Helper()
		w := doOn(t, r, http.MethodGet, "/v1/space/"+spaceA+"/projects?keyword=needle&"+query, token, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, total, w.Header().Get("X-Total-Count"))
		var rows []*Resp
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ProjectID)
		}
		return ids
	}
	first := read("page=1&limit=2", "3")
	second := read("page=2&limit=2", "3")
	require.Len(t, first, 2)
	require.Len(t, second, 1)
	require.ElementsMatch(t, []string{nameOnly, descriptionOnly, both}, append(first, second...))
	require.Empty(t, read("page=3&limit=2", "3"))
	require.Equal(t, []string{archived}, read("archive_status=archived&page=1&limit=20", "1"))
	require.ElementsMatch(t, []string{nameOnly, descriptionOnly, both, archived}, read("archive_status=all&page=1&limit=20", "4"))
}

func TestProjectDirectorySearchDescriptionLiteralCharacters(t *testing.T) {
	_, p := setup(t)
	uid := "directory-literal-owner"
	seedUser(t, uid)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, uid, 0, 1)
	for _, tc := range []struct{ name, description string }{
		{"literal", "100% a_b ! 中文"},
		{"wildcard decoy", "1000 axb plain"},
	} {
		_, err := p.createProject(createInput{SpaceID: spaceA, Creator: uid, Name: tc.name, Description: tc.description})
		require.NoError(t, err)
	}
	for _, keyword := range []string{"100%", "a_b", "!", "中文"} {
		t.Run(keyword, func(t *testing.T) {
			result, err := p.readProjects(spaceA, uid, keyword, projectReadPage{Limit: 20}, projectArchiveActive)
			require.NoError(t, err)
			require.EqualValues(t, 1, result.Total)
			require.Len(t, result.Rows, 1)
			require.Equal(t, "literal", result.Rows[0].Name)
		})
	}
}
