package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// TestBatchUserItemWhitelist locks the privacy contract of POST /v1/users/batch:
// the response DTO exposes exactly uid / name / avatar and never the sensitive
// Phone / Email fields that the service-layer Resp carries.
func TestBatchUserItemWhitelist(t *testing.T) {
	rt := reflect.TypeOf(batchUserItem{})
	tags := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		tags[rt.Field(i).Tag.Get("json")] = true
	}
	if rt.NumField() != 3 {
		t.Fatalf("batchUserItem must expose exactly 3 fields, got %d", rt.NumField())
	}
	for _, want := range []string{"uid", "name", "avatar"} {
		if !tags[want] {
			t.Fatalf("batchUserItem missing %q json field", want)
		}
	}
	for _, banned := range []string{"phone", "email", "Phone", "Email"} {
		if tags[banned] {
			t.Fatalf("batchUserItem must not expose %q", banned)
		}
	}
}

// TestBatchGetUsersRejectsOversizedBatch exercises the >200 uid guard. The cap
// check runs before any service / DB access, so a zero-value *User is enough to
// drive this path without a test server.
func TestBatchGetUsersRejectsOversizedBatch(t *testing.T) {
	u := &User{}
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.POST("/v1/users/batch", u.batchGetUsers)

	uids := make([]string, maxBatchUserUIDs+1)
	for i := range uids {
		uids[i] = "u"
	}
	body, err := json.Marshal(batchUserReq{UIDs: uids})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/users/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	env := decodeEnvelope(t, w.Body.Bytes())
	if env.Error.Code != "err.server.user.request_invalid" {
		t.Fatalf("want err.server.user.request_invalid, got %q (body=%s)", env.Error.Code, w.Body.String())
	}
}
