package search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestSearchNoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	names, err := os.ReadFile("api_names.go")
	if err != nil {
		t.Fatal(err)
	}
	cleaned := stripSearchLineComments(string(data) + "\n" + string(names))
	for _, b := range []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", ".AbortWithStatusJSON(", ".AbortWithStatus(", ".ResponseWithStatus(", "c.Response(\""} {
		if strings.Contains(cleaned, b) {
			t.Fatalf("modules/search/api.go must use httperr.ResponseErrorL instead of legacy %s", b)
		}
	}
	for _, line := range strings.Split(cleaned, "\n") {
		if strings.Contains(line, "c.JSON(http.Status") && !strings.Contains(line, "c.JSON(http.StatusOK") {
			t.Fatalf("modules/search/api.go must not use raw non-OK c.JSON: %s", strings.TrimSpace(line))
		}
	}
}

func TestRespondSearchHelpers(t *testing.T) {
	cases := []struct {
		name     string
		probe    func(c *wkhttp.Context)
		wantCode string
		wantHTTP int
	}{
		{
			name:     "request_invalid",
			probe:    func(c *wkhttp.Context) { respondSearchRequestInvalid(c, "keyword") },
			wantCode: "err.server.search.request_invalid",
			wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "message_query_failed",
			probe:    func(c *wkhttp.Context) { httperr.ResponseErrorL(c, errcode.ErrSearchMessageQueryFailed, nil, nil) },
			wantCode: "err.server.search.message_query_failed",
			wantHTTP: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := exerciseSearchHelper(t, tc.probe)
			if env.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.HTTPStatus != tc.wantHTTP {
				t.Fatalf("http_status = %d, want %d", env.Error.HTTPStatus, tc.wantHTTP)
			}
		})
	}
}

type searchErrEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func exerciseSearchHelper(t *testing.T, probe func(c *wkhttp.Context)) searchErrEnvelope {
	t.Helper()
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET("/probe", probe)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	r.ServeHTTP(w, req)
	var env searchErrEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v\nbody: %s", err, w.Body.String())
	}
	return env
}

func stripSearchLineComments(src string) string {
	var clean strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	return clean.String()
}
