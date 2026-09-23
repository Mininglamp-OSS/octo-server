package project

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

func botReadTestRouter(p *Project) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.GET("/v1/bot/projects", p.botListProjects)
	r.GET("/v1/bot/projects/:project_id", p.botGetProject)
	r.GET("/v1/bot/projects/:project_id/members", p.botListProjectMembers)
	return r
}

func TestBotProjectReadsRequireExplicitOBOAndRejectImpersonation(t *testing.T) {
	p := &Project{}
	r := botReadTestRouter(p)
	cases := []struct {
		name string
		path string
		want int
	}{
		{"list requires obo", "/v1/bot/projects?space_id=space-1", http.StatusBadRequest},
		{"detail rejects human uid", "/v1/bot/projects/project-1?obo=true&space_id=space-1&human_uid=human-1", http.StatusBadRequest},
		{"members rejects subject uid", "/v1/bot/projects/project-1/members?obo=true&space_id=space-1&subject_uid=human-1", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			res := httptest.NewRecorder()
			r.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d, want %d; body=%s", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

func TestBotProjectReadsRequireBotBearerCredential(t *testing.T) {
	p := &Project{}
	r := botReadTestRouter(p)
	req := httptest.NewRequest(http.MethodGet, "/v1/bot/projects?obo=true&space_id=space-1", nil)
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d; body=%s", res.Code, http.StatusUnauthorized, res.Body.String())
	}
}
