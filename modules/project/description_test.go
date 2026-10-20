package project

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectDescriptionRoundTripsAtFleetByteLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		create  string
		updated string
	}{
		{"ascii", strings.Repeat("a", 32768), strings.Repeat("b", 32768)},
		{"multibyte", strings.Repeat("界", 10922) + "ab", strings.Repeat("🙂", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := setup(t)
			seedSpace(t, spaceA, 1)
			token := seedUser(t, "owner1")
			seedSpaceMember(t, spaceA, "owner1", 0, 1)

			w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
				map[string]any{"name": "description", "description": tc.create})
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			created := decodeResp(t, w)
			path := "/v1/projects/" + created.ProjectID
			w = doJSON(t, srv, http.MethodGet, path, token, nil)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tc.create, decodeResp(t, w).Description)

			w = doJSON(t, srv, http.MethodPut, path, token,
				map[string]any{"description": tc.updated})
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			w = doJSON(t, srv, http.MethodGet, path, token, nil)
			require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tc.updated, decodeResp(t, w).Description)
		})
	}
}

func TestProjectDescriptionRejectsBytesBeyondFleetLimit(t *testing.T) {
	srv, _ := setup(t)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "description", "description": "original"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	created := decodeResp(t, w)
	path := "/v1/projects/" + created.ProjectID
	oversize := strings.Repeat("界", 10923)

	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/space/" + spaceA + "/projects"},
		{http.MethodPut, path},
	} {
		t.Run(endpoint.method, func(t *testing.T) {
			w := doJSON(t, srv, endpoint.method, endpoint.path, token,
				map[string]any{"name": "oversize", "description": oversize})
			assertProjectErrorCode(t, w, "err.server.project.field_too_long")
			envelope := decodeProjectEnvelope(t, w.Body.Bytes())
			assert.Equal(t, "description", envelope.Error.Details["field"])
			assert.Equal(t, float64(32768), envelope.Error.Details["max_bytes"])
			assert.NotContains(t, envelope.Error.Details, "max_chars")
		})
	}

	w = doJSON(t, srv, http.MethodGet, path, token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "original", decodeResp(t, w).Description)
}

func TestProjectDescriptionMigrationPreservesText(t *testing.T) {
	srv, _ := setup(t)
	db, err := sql.Open("mysql", projectTestMySQLAddr())
	require.NoError(t, err)
	defer db.Close()
	const migration = "sql/20261020000001_project_description_text.sql"
	defer applyOneProjectMigrationFile(t, db, migration, migrateUp)

	_, err = db.Exec("ALTER TABLE octo_project MODIFY COLUMN description VARCHAR(500) NOT NULL DEFAULT ''")
	require.NoError(t, err)
	seedSpace(t, spaceA, 1)
	token := seedUser(t, "owner1")
	seedSpaceMember(t, spaceA, "owner1", 0, 1)
	original := strings.Repeat("界", 500)
	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceA+"/projects", token,
		map[string]any{"name": "migration", "description": original})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	path := "/v1/projects/" + decodeResp(t, w).ProjectID

	applyOneProjectMigrationFile(t, db, migration, migrateUp)
	w = doJSON(t, srv, http.MethodGet, path, token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, original, decodeResp(t, w).Description)

	long := strings.Repeat("🙂", 8192)
	w = doJSON(t, srv, http.MethodPut, path, token, map[string]any{"description": long})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	applyOneProjectMigrationFile(t, db, migration, migrateDown)
	w = doJSON(t, srv, http.MethodGet, path, token, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, long, decodeResp(t, w).Description)
}
