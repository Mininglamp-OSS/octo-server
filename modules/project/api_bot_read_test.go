package project

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/obo"
)

type botProjectSnapshotReader struct {
	snapshot obo.Snapshot
	err      error
	calls    *int
}

func (r botProjectSnapshotReader) Read(context.Context, string, string, obo.Mode) (obo.Snapshot, error) {
	if r.calls != nil {
		(*r.calls)++
	}
	return r.snapshot, r.err
}

func botReadTestRouter(p *Project) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.GET("/v1/bot/projects", p.botListProjects)
	r.GET("/v1/bot/projects/:project_id", p.botGetProject)
	r.GET("/v1/bot/projects/:project_id/members", p.botListProjectMembers)
	return r
}

func botReadTestRegistry(t *testing.T) *obo.ActionRegistry {
	t.Helper()
	registry, err := obo.ParseActionRegistry(`{"all":["ALL"]}`)
	if err != nil {
		t.Fatal(err)
	}
	return registry
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

func TestBotProjectReadsFailClosedWhenActionRegistryIsEmpty(t *testing.T) {
	registry, err := obo.ParseActionRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := &Project{oboRegistry: registry, oboReader: botProjectSnapshotReader{
		calls: &calls,
		snapshot: obo.Snapshot{BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7,
			BoundScopes: []string{"ALL"}},
	}}
	r := botReadTestRouter(p)
	req := httptest.NewRequest(http.MethodGet, "/v1/bot/projects?obo=true&space_id=space-1", nil)
	req.Header.Set("Authorization", "Bearer bf_valid")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("status=%d calls=%d; body=%s", res.Code, calls, res.Body.String())
	}
}

func TestBotProjectReadsFailClosedOnDelegationDenials(t *testing.T) {
	cases := []struct {
		name   string
		reader obo.SnapshotReader
	}{
		{"missing grant", botProjectSnapshotReader{snapshot: obo.Snapshot{BotUID: "bot-1", OwnerUID: "human-1"}}},
		{"missing ALL binding", botProjectSnapshotReader{snapshot: obo.Snapshot{BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7}}},
		{"owner not in Space", botProjectSnapshotReader{err: &obo.DecisionError{Code: "space_not_allowed", Status: http.StatusForbidden}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Project{oboReader: tc.reader, oboRegistry: botReadTestRegistry(t)}
			r := botReadTestRouter(p)
			req := httptest.NewRequest(http.MethodGet, "/v1/bot/projects?obo=true&space_id=space-1", nil)
			req.Header.Set("Authorization", "Bearer bf_valid")
			res := httptest.NewRecorder()
			r.ServeHTTP(res, req)
			if res.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want %d; body=%s", res.Code, http.StatusForbidden, res.Body.String())
			}
		})
	}
}

func TestBotProjectPrincipalUsesOwnerAsSubject(t *testing.T) {
	p := &Project{Log: log.NewTLog("Project"), oboRegistry: botReadTestRegistry(t), oboReader: botProjectSnapshotReader{snapshot: obo.Snapshot{
		BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7, PolicyVersion: 3, BoundScopes: []string{"ALL"},
	}}}
	r := wkhttp.New()
	r.GET("/probe", func(c *wkhttp.Context) {
		principal, ok := p.botOBOPrincipal(c, "all")
		if !ok {
			return
		}
		c.Response(map[string]string{"actor": principal.Actor.UID, "subject": principal.Subject.UID})
	})
	req := httptest.NewRequest(http.MethodGet, "/probe?obo=true&space_id=space-1", nil)
	req.Header.Set("Authorization", "Bearer bf_valid")
	res := httptest.NewRecorder()
	r.ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"actor":"bot-1"`) ||
		!strings.Contains(res.Body.String(), `"subject":"human-1"`) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}
