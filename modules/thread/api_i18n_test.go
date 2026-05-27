package thread

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
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

// TestClassifyThreadError pins the current substring-based contract between
// service.go / db.go error strings and the corresponding errcode. If a service
// error message changes, this test should fail loudly rather than letting the
// classifier silently fall through to ErrThreadStoreFailed (500).
func TestClassifyThreadError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want codes.Code
	}{
		{"nil", "", errcode.ErrThreadStoreFailed},
		{"not a group member", "not a group member", errcode.ErrThreadNotGroupMember},
		{"no permission", "no permission to edit", errcode.ErrThreadPermissionDenied},
		{"creator cannot leave", "creator cannot leave thread", errcode.ErrThreadCreatorCannotLeave},
		{"status changed concurrently", "thread status changed concurrently", errcode.ErrThreadStatusChanged},
		{"thread is not active", "thread is not active", errcode.ErrThreadNotActive},
		{"thread has been deleted", "thread has been deleted", errcode.ErrThreadDeleted},
		{"thread not found", "thread not found", errcode.ErrThreadNotFound},
		// DB layer ambiguous string maps to 404 NotFound (the more accurate
		// default for clients hitting an unknown shortID).
		{"db ambiguous not found or already deleted", "thread not found or already deleted", errcode.ErrThreadNotFound},
		{"name required", "name is required and must not exceed 100 characters", errcode.ErrThreadNameInvalid},
		{"invalid mute value", "invalid mute value type", errcode.ErrThreadSettingInvalid},
		{"mute must", "mute must be 0 or 1", errcode.ErrThreadSettingInvalid},
		// Mixed case input must still classify — classifier lowercases first.
		{"mixed case still matches", "Thread Not Found", errcode.ErrThreadNotFound},
		// Anything unrecognised falls through to a 500 store failure.
		{"unknown driver error", "driver: bad connection", errcode.ErrThreadStoreFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.in != "" {
				err = errors.New(tc.in)
			}
			got := classifyThreadError(err)
			if got.ID != tc.want.ID {
				t.Fatalf("classifyThreadError(%q) = %s, want %s", tc.in, got.ID, tc.want.ID)
			}
		})
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
