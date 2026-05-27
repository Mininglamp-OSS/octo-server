package thread

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func TestThreadAPINoLegacyResponseError(t *testing.T) {
	data, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	if strings.Contains(string(data), ".ResponseError(") {
		t.Fatal("modules/thread/api.go must use httperr.ResponseErrorL instead of legacy ResponseError")
	}
}

func TestThreadAPIInvalidGroupNoUsesLocalizedEnvelope(t *testing.T) {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	th := &Thread{}
	r.POST("/v1/groups/:group_no/threads", th.createThread)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/groups/not-a-group/threads", strings.NewReader(`{"name":"topic"}`))
	req.Header.Set("Accept-Language", "zh-CN")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("HTTP status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Language"); got != "zh-CN" {
		t.Fatalf("Content-Language = %q, want zh-CN", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("localized error object missing: %#v", body)
	}
	if got := errObj["code"]; got != errcode.ErrThreadGroupNoInvalid.ID {
		t.Fatalf("error.code = %q, want %q", got, errcode.ErrThreadGroupNoInvalid.ID)
	}
	if got := errObj["message"]; got != "群编号无效。" {
		t.Fatalf("error.message = %q", got)
	}
	if got := body["msg"]; got != errObj["message"] {
		t.Fatalf("legacy msg = %q, want %q", got, errObj["message"])
	}
}
