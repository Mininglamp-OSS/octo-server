package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/obo"
)

type authResolveSnapshotReader struct {
	state    obo.Snapshot
	calls    int
	botToken string
	spaceID  string
	mode     obo.Mode
}

func (r *authResolveSnapshotReader) Read(_ context.Context, botToken, spaceID string, mode obo.Mode) (obo.Snapshot, error) {
	r.calls++
	r.botToken = botToken
	r.spaceID = spaceID
	r.mode = mode
	return r.state, nil
}

func newAuthResolveTestRoute(t *testing.T, reader obo.SnapshotReader, registryJSON string) *wkhttp.WKHttp {
	t.Helper()
	registry, err := obo.ParseActionRegistry(registryJSON)
	if err != nil {
		t.Fatal(err)
	}
	u := &User{
		Log:         log.NewTLog("auth-resolve-test"),
		oboReader:   reader,
		oboRegistry: registry,
	}
	route := wkhttp.New()
	route.POST("/v1/internal/auth/resolve", u.authResolveBot)
	return route
}

func postAuthResolve(t *testing.T, route *wkhttp.WKHttp, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/internal/auth/resolve", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	route.ServeHTTP(recorder, request)
	return recorder
}

func TestAuthResolveBotUsesExternalActionRegistry(t *testing.T) {
	reader := &authResolveSnapshotReader{state: obo.Snapshot{
		BotUID: "bot-1", OwnerUID: "human-1", GrantID: 7, PolicyVersion: 4,
		BoundScopes: []string{"ALL"},
	}}
	route := newAuthResolveTestRoute(t, reader, `{"all":["ALL"]}`)
	recorder := postAuthResolve(t, route,
		`{"bot_token":"bf_ExactToken","mode":"OBO","space_id":"S","action":"all"}`)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", recorder.Header().Get("Cache-Control"))
	}
	var principal obo.Principal
	if err := json.Unmarshal(recorder.Body.Bytes(), &principal); err != nil {
		t.Fatal(err)
	}
	if principal.Mode != obo.ModeOBO || principal.Actor.UID != "bot-1" || principal.Subject.UID != "human-1" {
		t.Fatalf("principal=%+v", principal)
	}
	if principal.Delegation == nil || principal.Delegation.Action != "all" || principal.Delegation.PolicyVersion != 4 {
		t.Fatalf("delegation=%+v", principal.Delegation)
	}
	if reader.calls != 1 || reader.botToken != "bf_ExactToken" || reader.spaceID != "S" || reader.mode != obo.ModeOBO {
		t.Fatalf("reader calls=%d token=%q space=%q mode=%q", reader.calls, reader.botToken, reader.spaceID, reader.mode)
	}
}

func TestAuthResolveBotRejectsUnregisteredActionBeforeSnapshotRead(t *testing.T) {
	reader := &authResolveSnapshotReader{state: obo.Snapshot{BotUID: "bot-1"}}
	route := newAuthResolveTestRoute(t, reader, `{"all":["ALL"]}`)
	recorder := postAuthResolve(t, route,
		`{"bot_token":"bf_ExactToken","mode":"OBO","space_id":"S","action":"project.read"}`)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "action_not_allowed" || reader.calls != 0 {
		t.Fatalf("response=%+v reader calls=%d", response, reader.calls)
	}
}
