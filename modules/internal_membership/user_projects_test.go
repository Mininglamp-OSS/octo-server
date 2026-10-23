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
	"fmt"
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

// TestProjectListRejectsNonSingleOrPaddedSelectors covers malformed selector
// shapes: missing or malformed Space/user selectors. Key presence is tested by
// the multi-mode selector cases below.
func TestProjectListRejectsNonSingleOrPaddedSelectors(t *testing.T) {
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
		{"missing space_id in ID mode", "?project_ids=project-a", "space_id"},
		{"repeated space_id in ID mode", "?space_id=space-a&space_id=space-b&project_ids=project-a", "space_id"},
		{"missing both selectors", "?space_id=space-a", "uid|project_ids"},
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
			require.Zero(t, s.metadataCalls,
				"a malformed selector must be rejected before any metadata lookup")
		})
	}
}

func TestProjectListRejectsConflictingSelectors(t *testing.T) {
	for _, query := range []string{
		"?space_id=space-a&uid=User-A&project_ids=project-a",
		"?space_id=space-a&uid=User-A&project_ids=",
		"?space_id=space-a&uid=&project_ids=project-a",
	} {
		t.Run(query, func(t *testing.T) {
			store := &stubStore{}
			response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken, query)

			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			env := decodeError(t, response)
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, env.Error.Code)

			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, "uid|project_ids", body.Error.Details["field"])
			require.Zero(t, store.projectCalls,
				"conflicting selectors must be rejected before any data lookup")
			require.Zero(t, store.metadataCalls,
				"conflicting selectors must be rejected before any metadata lookup")
		})
	}
}

func TestProjectListRejectsEmptyAndOversizedProjectIDSelectors(t *testing.T) {
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = fmt.Sprintf("project-%02d", i)
	}
	cases := []struct {
		name  string
		query string
	}{
		{"empty", "?space_id=space-a&project_ids=,,%20"},
		{"over 50 unique IDs", "?space_id=space-a&project_ids=" + strings.Join(ids, ",")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{}
			response := doGetProjects(t, newRouter(newTestModule(store)), testInternalToken, tc.query)

			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			env := decodeError(t, response)
			require.Equal(t, errcode.ErrSharedParamInvalid.ID, env.Error.Code)

			var body struct {
				Error struct {
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			require.Equal(t, "project_ids", body.Error.Details["field"])
			require.Zero(t, store.projectCalls,
				"invalid project ID selectors must be rejected before any data lookup")
			require.Zero(t, store.metadataCalls,
				"invalid project ID selectors must be rejected before any metadata lookup")
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
