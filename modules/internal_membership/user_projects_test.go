package internal_membership

// Behaviour tests for GET /v1/internal/projects.
//
// Scope is deliberately narrow: selector strictness and the 401/500 boundaries the
// handler itself owns. Activation, Space isolation, ordering, schema and empty
// results are pkg/project's contract and are covered by modules/project's real
// database HTTP test against the same production route.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/stretchr/testify/require"
)

// doGetProjects is doGet for the projects endpoint; doGet is pinned to the epochs
// path and changing it would silently retarget every existing test.
func doGetProjects(t *testing.T, r *wkhttp.WKHttp, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, projectsPath+query, nil)
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestUserProjectsRejectsNonSingleOrPaddedSelectors covers every malformed
// selector shape the contract names: missing, empty, padded on either side, and
// repeated. The repeated case matters most: accepting one of the values would
// answer a question the caller did not unambiguously ask.
func TestUserProjectsRejectsNonSingleOrPaddedSelectors(t *testing.T) {
	cases := []struct {
		name  string
		query string
		field string
	}{
		{"missing space_id", "?uid=User-A", "space_id"},
		{"empty space_id", "?space_id=&uid=User-A", "space_id"},
		{"leading space in space_id", "?space_id=%20space-a&uid=User-A", "space_id"},
		{"trailing space in space_id", "?space_id=space-a%20&uid=User-A", "space_id"},
		{"repeated space_id", "?space_id=space-a&space_id=space-b&uid=User-A", "space_id"},
		{"missing uid", "?space_id=space-a", "uid"},
		{"empty uid", "?space_id=space-a&uid=", "uid"},
		{"leading space in uid", "?space_id=space-a&uid=%20User-A", "uid"},
		{"trailing space in uid", "?space_id=space-a&uid=User-A%20", "uid"},
		{"repeated uid", "?space_id=space-a&uid=User-A&uid=User-B", "uid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{}
			w := doGetProjects(t, newRouter(newTestModule(s)), testInternalToken, tc.query)

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			env := decodeError(t, w)
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, env.Error.Code)
			require.Equal(t, http.StatusBadRequest, env.Error.HTTPStatus)

			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tc.field, body.Error.Details["field"],
				"the 400 must name the offending field so a peer can fix its request")

			require.Zero(t, s.projectCalls,
				"a malformed selector must be rejected before any database work")
		})
	}
}

// TestUserProjectsFailureIsNotAnEmptyList is the fail-closed half of the contract.
// An empty 200 would tell the consumer "this user is in no projects", which is a
// very different fact from "the read failed".
func TestUserProjectsFailureIsNotAnEmptyList(t *testing.T) {
	s := &stubStore{projErr: errors.New("db down: connection refused")}
	w := doGetProjects(t, newRouter(newTestModule(s)), testInternalToken,
		"?space_id=space-a&uid=User-A")

	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	env := decodeError(t, w)
	require.Equal(t, errcode.ErrSharedInternal.ID, env.Error.Code)
	require.Equal(t, http.StatusInternalServerError, env.Error.HTTPStatus)
	require.NotEqual(t, "[]", strings.TrimSpace(w.Body.String()),
		"a failed lookup must never be rendered as a successful empty array")
	require.NotContains(t, w.Body.String(), "db down",
		"the internal failure reason belongs in the log, not on the wire")
	require.Equal(t, 1, s.projectCalls)
}
