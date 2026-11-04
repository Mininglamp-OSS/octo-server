package project

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

func projectErrorResponseForPrivacyTest(
	t *testing.T, code codes.Code, unsafeRawErr, unsafeDetail string,
) (*httptest.ResponseRecorder, projectErrEnvelope) {
	t.Helper()
	r := projectHelperHarness(func(c *wkhttp.Context) {
		httperr.ResponseErrorL(c, code, nil, i18n.Details{
			"raw_err":        unsafeRawErr,
			"private_detail": unsafeDetail,
		})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return w, decodeProjectEnvelope(t, w.Body.Bytes())
}

// The public readiness refusal must retain its distinct semantic status 503,
// while genuine internal failures share the generic response. Both paths must
// discard values a caller might accidentally pass as raw error/detail data.
func TestProjectPublicErrorResponsePreservesCategoryAndPrivacy(t *testing.T) {
	const (
		unsafeRawErr = "private database diagnostic octows-secret"
		unsafeDetail = "private transport detail octows-secret"
	)

	cases := []struct {
		name       string
		code       codes.Code
		wantStatus int
	}{
		{name: "not ready", code: errcode.ErrProjectNotReady, wantStatus: http.StatusServiceUnavailable},
		{name: "query failure", code: errcode.ErrProjectQueryFailed, wantStatus: http.StatusInternalServerError},
		{name: "store failure", code: errcode.ErrProjectStoreFailed, wantStatus: http.StatusInternalServerError},
	}
	responses := make(map[string]projectErrEnvelope, len(cases))
	for _, tc := range cases {
		w, envelope := projectErrorResponseForPrivacyTest(t, tc.code, unsafeRawErr, unsafeDetail)
		if got := envelope.Error.Code; got != tc.code.ID {
			t.Errorf("%s response code = %q, want %q", tc.name, got, tc.code.ID)
		}
		if got := envelope.Error.HTTPStatus; got != tc.wantStatus {
			t.Errorf("%s semantic status = %d, want %d", tc.name, got, tc.wantStatus)
		}
		if strings.Contains(w.Body.String(), unsafeRawErr) || strings.Contains(w.Body.String(), unsafeDetail) {
			t.Errorf("%s response leaked unsafe error/detail values: %s", tc.name, w.Body.String())
		}
		if _, ok := envelope.Error.Details["raw_err"]; ok {
			t.Errorf("%s response exposed raw_err details: %#v", tc.name, envelope.Error.Details)
		}
		if _, ok := envelope.Error.Details["private_detail"]; ok {
			t.Errorf("%s response exposed private details: %#v", tc.name, envelope.Error.Details)
		}
		responses[tc.name] = envelope
	}

	if responses["not ready"].Error.Message == responses["query failure"].Error.Message {
		t.Error("public not_ready response rendered like a generic internal failure")
	}
	if responses["query failure"].Error.Message != responses["store failure"].Error.Message {
		t.Error("query and store failures no longer share the generic internal response")
	}
}
