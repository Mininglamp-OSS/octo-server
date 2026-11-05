package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

func TestNameSearchHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body, space string
		failure           error
		code              string
		calls             int
	}{
		{"valid", `{"keyword":"向","entry":"global","space_id":"A","uid":"attacker"}`, "A", nil, "", 1},
		{"empty", `{"keyword":"","entry":"global"}`, "", nil, "err.server.search.request_invalid", 0},
		{"space_mismatch", `{"keyword":"向","entry":"global","space_id":"B"}`, "A", nil, "err.server.search.request_invalid", 0},
		{"explicit_null_with_header", `{"keyword":"向","entry":"global","space_id":null}`, "A", nil, "err.server.search.request_invalid", 0},
		{"source_failure", `{"keyword":"向","entry":"global"}`, "", errors.New("db down"), "err.server.search.name_query_failed", 1},
		{"query_changed", `{"keyword":"向","entry":"global"}`, "", errNameQueryChanged, "err.server.search.query_changed", 1},
		{"forbidden", `{"keyword":"向","entry":"global"}`, "", errNameForbidden, "err.server.search.name_forbidden", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := wkhttp.New()
			r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
			r.POST("/names", func(c *wkhttp.Context) {
				c.Set("uid", "caller")
				spacepkg.SetSpaceID(c, tc.space)
				serveNameSearch(c, "contact", func(_ context.Context, uid, kind string, q nameQuery) (nameCandidateSet, error) {
					calls++
					if uid != "caller" {
						t.Errorf("untrusted uid %s", uid)
					}
					return nameCandidateSet{Complete: true}, tc.failure
				})
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/names", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if calls != tc.calls {
				t.Fatalf("calls %d want %d body=%s", calls, tc.calls, w.Body.String())
			}
			var body map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if tc.code == "" {
				if w.Code != 200 || body["complete"] != true || body["total"] != float64(0) {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			} else if !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("expected %s: %s", tc.code, w.Body.String())
			}
		})
	}
}
