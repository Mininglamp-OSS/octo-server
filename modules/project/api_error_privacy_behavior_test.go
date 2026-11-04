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
	t *testing.T, code codes.Code, unsafeDetails i18n.Details,
) (*httptest.ResponseRecorder, projectErrEnvelope) {
	t.Helper()
	r := projectHelperHarness(func(c *wkhttp.Context) {
		httperr.ResponseErrorL(c, code, nil, unsafeDetails)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return w, decodeProjectEnvelope(t, w.Body.Bytes())
}

// This exercises current registry entries through the real HTTP renderer; it
// is not an enumeration of error-producing storage or SQL writers. A newly
// registered Project 5xx that exposes its own diagnostic message or unsafe
// details therefore fails here without inspecting implementation markers.
func TestProjectRegistered5xxErrorsRenderGenericExceptNotReady(t *testing.T) {
	const projectIDPrefix = "err.server.project."
	const (
		unsafeRawErr = "private database diagnostic octows-secret"
		unsafeUID    = "private uid octows-secret"
		unsafeToken  = "private token octows-secret"
		unsafeDetail = "private transport detail octows-secret"
	)
	unsafeDetails := i18n.Details{
		"raw_err":        unsafeRawErr,
		"uid":            unsafeUID,
		"token":          unsafeToken,
		"private_detail": unsafeDetail,
	}
	unsafeValues := []string{unsafeRawErr, unsafeUID, unsafeToken, unsafeDetail}
	unsafeKeys := []string{"raw_err", "uid", "token", "private_detail"}

	genericWire, generic := projectErrorResponseForPrivacyTest(t, errcode.ErrSharedInternal, unsafeDetails)
	if genericWire.Code != http.StatusBadRequest || generic.Status != http.StatusBadRequest {
		t.Errorf("shared internal wire/status = %d/%d, want legacy 400/400",
			genericWire.Code, generic.Status)
	}
	if generic.Error.Code != errcode.ErrSharedInternal.ID {
		t.Errorf("shared internal response code = %q, want %q", generic.Error.Code, errcode.ErrSharedInternal.ID)
	}
	if generic.Error.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("shared internal semantic status = %d, want 500", generic.Error.HTTPStatus)
	}

	foundNotReady := false
	for _, code := range codes.All() {
		if !strings.HasPrefix(code.ID, projectIDPrefix) || code.HTTPStatus < 500 || code.HTTPStatus > 599 {
			continue
		}
		t.Run(code.ID, func(t *testing.T) {
			w, envelope := projectErrorResponseForPrivacyTest(t, code, unsafeDetails)
			if w.Code != http.StatusBadRequest || envelope.Status != http.StatusBadRequest {
				t.Errorf("wire/status = %d/%d, want legacy 400/400", w.Code, envelope.Status)
			}
			if envelope.Error.Code != code.ID {
				t.Errorf("response code = %q, want %q", envelope.Error.Code, code.ID)
			}
			if envelope.Error.HTTPStatus != code.HTTPStatus {
				t.Errorf("semantic status = %d, want %d", envelope.Error.HTTPStatus, code.HTTPStatus)
			}
			if envelope.Msg != envelope.Error.Message {
				t.Errorf("legacy msg = %q, nested message = %q", envelope.Msg, envelope.Error.Message)
			}
			for _, value := range unsafeValues {
				if strings.Contains(w.Body.String(), value) {
					t.Errorf("response leaked unsafe value %q: %s", value, w.Body.String())
				}
			}
			for _, key := range unsafeKeys {
				if _, ok := envelope.Error.Details[key]; ok {
					t.Errorf("response exposed unsafe detail %q: %#v", key, envelope.Error.Details)
				}
			}
			if len(envelope.Error.Details) != 0 {
				t.Errorf("response contains details, want none: %#v", envelope.Error.Details)
			}

			if code.ID == errcode.ErrProjectNotReady.ID {
				foundNotReady = true
				if envelope.Error.HTTPStatus != http.StatusServiceUnavailable {
					t.Errorf("not_ready semantic status = %d, want 503", envelope.Error.HTTPStatus)
				}
				if envelope.Error.Message == generic.Error.Message {
					t.Errorf("not_ready message = %q, want a distinct public readiness prompt", envelope.Error.Message)
				}
				return
			}

			if envelope.Error.Message != generic.Error.Message {
				t.Errorf("Project 5xx message = %q, want the shared generic internal response", envelope.Error.Message)
			}
		})
	}
	if !foundNotReady {
		t.Fatal("registered Project 5xx errors did not include the public not_ready exception")
	}
}
