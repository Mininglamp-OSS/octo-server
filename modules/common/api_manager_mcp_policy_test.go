package common

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestMCPPolicyDefaultAdminGate(t *testing.T) {
	for _, role := range []string{"", string(wkhttp.Admin), "group_admin", string(wkhttp.SuperAdmin)} {
		r := wkhttp.New()
		r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
		m := &Manager{}
		r.Group("").PUT("/probe", func(c *wkhttp.Context) { c.Set("role", role); m.putMCPPolicyDefault(c) })
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/probe", strings.NewReader(`{}`)))
		var env struct {
			Error struct {
				Code       string `json:"code"`
				HTTPStatus int    `json:"http_status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 400 || rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal(rec.Body.String())
		}
		if role == string(wkhttp.SuperAdmin) {
			if env.Error.Code != "err.server.mcp.policy_invalid" {
				t.Fatal(rec.Body.String())
			}
		} else if env.Error.Code != "err.shared.auth.forbidden" || env.Error.HTTPStatus != 403 {
			t.Fatal(rec.Body.String())
		}
	}
}

func TestMCPPolicyManagerNoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api_manager_mcp_policy.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{".ResponseError(", ".ResponseErrorf(", ".AbortWithStatusJSON(", ".ResponseErrorWithStatus("} {
		if strings.Contains(string(data), banned) {
			t.Fatal("legacy error response", banned)
		}
	}
}
