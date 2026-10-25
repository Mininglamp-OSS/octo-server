package internal_membership

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

func rosterGet(t *testing.T, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	// No config/DB is wired: rejected requests must stop before the data read.
	r := newRouter(newTestModule(nil))
	req := httptest.NewRequest(http.MethodGet, projectMembershipsPath+query, nil)
	req.Header.Set(internalTokenHeader, token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestProjectMembershipsRejectInvalidSelectorsBeforeDatabase(t *testing.T) {
	for _, query := range []string{
		"", "?space_id=s", "?project_id=p", "?space_id=&project_id=p", "?space_id=s&project_id=%20p",
		"?space_id=s&space_id=other&project_id=p", "?space_id=s&project_id=p&project_id=other",
		"?space_id=s&project_id=p&limit=1&limit=2", "?space_id=s&project_id=p&page=1&page=2",
		"?space_id=s&project_id=p&offset=1", "?space_id=s&project_id=p&uid=reader", "?space_id=s&project_id=%ZZ",
		"?space_id=" + strings.Repeat("s", 257) + "&project_id=p",
		"?space_id=s&project_id=" + strings.Repeat("p", 257),
	} {
		t.Run(query, func(t *testing.T) {
			w := rosterGet(t, testInternalToken, query)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, decodeError(t, w).Error.Code)
		})
	}
}

func TestProjectMembershipsRequireInternalCredential(t *testing.T) {
	for _, token := range []string{"", "wrong-token", "bf_not-an-internal-token"} {
		t.Run(token, func(t *testing.T) {
			w := rosterGet(t, token, "?space_id=s&project_id=p")
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
	}
}

// A closed real session must produce the normal localized read error with no
// membershipStore wired; framework recovery from a nil-store panic is not enough.
func TestProjectMembershipsReadDoesNotRequireStore(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	if addr := os.Getenv("OCTO_TEST_MYSQL_ADDR"); addr != "" {
		cfg.DB.MySQLAddr = addr
	}
	ctx := config.NewContext(cfg)
	require.NoError(t, ctx.DB().DB.Close())
	m := newTestModule(nil)
	m.ctx = ctx
	req := httptest.NewRequest(http.MethodGet, projectMembershipsPath+"?space_id=s&project_id=p", nil)
	req.Header.Set(internalTokenHeader, testInternalToken)
	w := httptest.NewRecorder()
	newRouter(m).ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.Equal(t, errcode.ErrSharedInternal.ID, decodeError(t, w).Error.Code)
	require.NotContains(t, w.Body.String(), "database is closed")
	require.Empty(t, w.Header().Get("X-Total-Count"))
}
