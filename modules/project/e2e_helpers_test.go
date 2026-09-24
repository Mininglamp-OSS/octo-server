// Package project_test keeps end-to-end HTTP helpers outside the Project
// package to use the complete module registry without an import cycle.
package project_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

// newE2EServer is testutil.NewTestServer plus the i18n error renderer.
//
// testutil.NewTestServer does not install one (main.go does), and without it a
// refused request falls back to the legacy {msg,status} body with no error.code —
// so an assertion on a code would silently compare against "" and pass for the
// wrong reason. api_test.go's TestMain wires it on the shared server for the same
// reason; these cases build their own, so they have to wire their own.
func newE2EServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	srv, ctx := testutil.NewTestServer()
	srv.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	return srv, ctx
}

// writeProjectE2E drives one authenticated JSON write through the real Project route.
func writeProjectE2E(
	t *testing.T,
	srv *server.Server,
	method, path, token string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// projectSeatStateE2E reads one Project seat straight from octo_project_member.
func projectSeatStateE2E(ctx *config.Context, projectID, uid string) (status, removing int, ok bool) {
	var rows []struct {
		Status   int `db:"status"`
		Removing int `db:"removing"`
	}
	_, err := ctx.DB().SelectBySql(
		"SELECT status, removing FROM octo_project_member WHERE project_id = ? AND uid = ?",
		projectID, uid,
	).Load(&rows)
	if err != nil || len(rows) == 0 {
		return 0, 0, false
	}
	return rows[0].Status, rows[0].Removing, true
}

// activeGroupMemberE2E reports whether the uid holds a live native membership.
func activeGroupMemberE2E(ctx *config.Context, groupNo, uid string) bool {
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND uid = ? "+
			"AND is_deleted = 0 AND status = 1",
		groupNo, uid,
	).Load(&uids)
	return err == nil && len(uids) == 1
}
