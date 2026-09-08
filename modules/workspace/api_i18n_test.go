package workspace

import (
	"encoding/json"
	"errors"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func workspaceProbe(probe func(*wkhttp.Context)) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	return r
}

type workspaceErrorEnvelope struct {
	Error struct {
		Code       string         `json:"code"`
		HTTPStatus int            `json:"http_status"`
		Details    map[string]any `json:"details"`
	} `json:"error"`
	Status int `json:"status"`
}

func TestRespondErrorMapsSentinelsToRegisteredCodes(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		code       string
		httpStatus int
	}{
		{"request_invalid", errors.Join(errors.New("wrapped"), ErrRequestInvalid), "err.server.workspace.request_invalid", http.StatusBadRequest},
		{"space_required", ErrSpaceRequired, "err.server.workspace.space_required", http.StatusBadRequest},
		{"forbidden", ErrForbidden, "err.server.workspace.forbidden", http.StatusForbidden},
		{"not_found", ErrNotFound, "err.server.workspace.not_found", http.StatusNotFound},
		{"owner_protected", ErrOwnerProtected, "err.server.workspace.owner_protected", http.StatusConflict},
		{"role_invalid", ErrRoleInvalid, "err.server.workspace.workspace_role_invalid", http.StatusUnprocessableEntity},
		{"candidate_ineligible", ErrCandidateIneligible, "err.server.workspace.candidate_ineligible", http.StatusUnprocessableEntity},
		{"dependency_unavailable", ErrDependencyUnavailable, "err.server.workspace.dependency_unavailable", http.StatusServiceUnavailable},
		{"unknown_internal", errors.New("database detail must stay private"), "err.server.workspace.internal", http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := workspaceProbe(func(c *wkhttp.Context) { RespondError(c, tc.err) })
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			r.ServeHTTP(rec, req)

			// D14 keeps the transport status at 400 while the envelope retains
			// the semantic status for clients and telemetry.
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var got workspaceErrorEnvelope
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, tc.code, got.Error.Code)
			require.Equal(t, tc.httpStatus, got.Error.HTTPStatus)
			require.Equal(t, http.StatusBadRequest, got.Status)
			if tc.name == "unknown_internal" {
				require.NotContains(t, rec.Body.String(), "database detail must stay private")
			}
		})
	}
}
