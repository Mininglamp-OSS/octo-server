package card_template_catalog

// PR-C Slice 2 RED (D2/D4 + Control APIs): the manager grant endpoints are
// superAdmin-only, control-gated, strict-body, principal-validated, and map
// every store outcome onto the localized envelope with the resulting
// revision in the success body.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

type fakeGrantCatalogStore struct {
	fakeCatalogStore
	upsertResult GrantResult
	upsertErr    error
	upsertCalls  int
	lastUpsert   UpsertGrantRequest
	revokeResult GrantResult
	revokeErr    error
	revokeCalls  int
	lastRevoke   RevokeGrantRequest
	grants       []GrantRecord
	grantsErr    error
}

func (s *fakeGrantCatalogStore) UpsertGrant(_ context.Context, request UpsertGrantRequest) (GrantResult, error) {
	s.upsertCalls++
	s.lastUpsert = request
	return s.upsertResult, s.upsertErr
}

func (s *fakeGrantCatalogStore) RevokeGrant(_ context.Context, request RevokeGrantRequest) (GrantResult, error) {
	s.revokeCalls++
	s.lastRevoke = request
	return s.revokeResult, s.revokeErr
}

func (s *fakeGrantCatalogStore) ListGrants(context.Context, cardtmpl.ID, int) ([]GrantRecord, bool, error) {
	return s.grants, false, s.grantsErr
}

func doGrantRequest(
	t *testing.T,
	api *API,
	role wkhttp.UserRole,
	method, target, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	route.Use(func(c *wkhttp.Context) {
		c.Set("uid", "admin-1")
		c.Set("role", string(role))
	})
	group := route.Group("")
	group.PUT("/:id/grants/:principal_type/:principal_id", api.grantUpsert)
	group.DELETE("/:id/grants/:principal_type/:principal_id", api.grantRevoke)
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, request)
	return recorder
}

func grantUpsertBody() string {
	return `{"scope_space_id":"space-1","discover":true,"send":true,"edit":false,` +
		`"expected_revision":0,"reason":"pilot rollout","change_ticket":"CHG-9"}`
}

func allowAllGrantPrincipals(context.Context, GrantIdentity) error { return nil }

func TestGrantWriteRequiresSuperAdminAndControlGate(t *testing.T) {
	store := &fakeGrantCatalogStore{}
	api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
	response := doGrantRequest(t, api, wkhttp.Admin, http.MethodPut,
		"/docs.access-request/grants/bot/bot-1", grantUpsertBody())
	assertCatalogError(t, response, "err.server.card_template_catalog.forbidden", http.StatusForbidden)

	api = &API{store: store, controlEnabled: false, validateGrantPrincipal: allowAllGrantPrincipals}
	response = doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/docs.access-request/grants/bot/bot-1", grantUpsertBody())
	assertCatalogError(t, response, "err.server.card_template_catalog.control_disabled", http.StatusServiceUnavailable)

	response = doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodDelete,
		"/docs.access-request/grants/bot/bot-1",
		`{"scope_space_id":"space-1","expected_revision":3,"reason":"offboard","change_ticket":"CHG-10"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.control_disabled", http.StatusServiceUnavailable)
	if store.upsertCalls != 0 || store.revokeCalls != 0 {
		t.Fatalf("gated requests reached store: %d/%d", store.upsertCalls, store.revokeCalls)
	}
}

func TestGrantUpsertHappyPathReturnsResultingRevision(t *testing.T) {
	store := &fakeGrantCatalogStore{upsertResult: GrantResult{
		Record: GrantRecord{
			Identity: GrantIdentity{
				TemplateID: "docs.access-request", PrincipalType: GrantPrincipalBot,
				PrincipalID: "bot-1", ScopeSpaceID: "space-1",
			},
			Status: GrantStatusActive, Permissions: GrantPermissions{Discover: true, Send: true},
			Revision: 1,
		},
		Created: true,
	}}
	api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
	response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/docs.access-request/grants/bot/bot-1", grantUpsertBody())
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.upsertCalls != 1 {
		t.Fatalf("upsert calls = %d", store.upsertCalls)
	}
	request := store.lastUpsert
	if request.Identity.PrincipalType != GrantPrincipalBot || request.Identity.PrincipalID != "bot-1" ||
		request.Identity.ScopeSpaceID != "space-1" || request.ActorUID != "admin-1" {
		t.Fatalf("store request = %+v", request)
	}
	body := response.Body.String()
	for _, needle := range []string{`"revision":1`, `"status":"active"`, `"created":true`, `"send":true`} {
		if !bytes.Contains([]byte(body), []byte(needle)) {
			t.Fatalf("response missing %s: %s", needle, body)
		}
	}
}

func TestGrantWriteStrictBodyRejections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "actor field rejected", body: `{"scope_space_id":"","discover":true,"send":false,"edit":false,` +
			`"expected_revision":0,"reason":"r","change_ticket":"t","updated_by":"spoof"}`},
		{name: "missing scope key", body: `{"discover":true,"send":false,"edit":false,` +
			`"expected_revision":0,"reason":"r","change_ticket":"t"}`},
		{name: "missing permission key", body: `{"scope_space_id":"","discover":true,"send":false,` +
			`"expected_revision":0,"reason":"r","change_ticket":"t"}`},
		{name: "null expected_revision", body: `{"scope_space_id":"","discover":true,"send":false,"edit":false,` +
			`"expected_revision":null,"reason":"r","change_ticket":"t"}`},
		{name: "dark grant send without discover", body: `{"scope_space_id":"","discover":false,"send":true,"edit":false,` +
			`"expected_revision":0,"reason":"r","change_ticket":"t"}`},
		{name: "blank reason", body: `{"scope_space_id":"","discover":true,"send":false,"edit":false,` +
			`"expected_revision":0,"reason":" ","change_ticket":"t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGrantCatalogStore{}
			api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
			response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
				"/docs.access-request/grants/bot/bot-1", tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if store.upsertCalls != 0 {
				t.Fatal("invalid body reached store")
			}
		})
	}
}

func TestGrantPathIdentityCanonicalValidation(t *testing.T) {
	cases := []struct {
		name   string
		target string
		body   string
	}{
		{name: "unknown principal type", target: "/docs.access-request/grants/user/u-1", body: grantUpsertBody()},
		{name: "system not grantable", target: "/docs.access-request/grants/system/readiness", body: grantUpsertBody()},
		{name: "malformed template id", target: "/DOCS!!/grants/bot/bot-1", body: grantUpsertBody()},
		{name: "space principal with exact scope", target: "/docs.access-request/grants/space/space-1",
			body: `{"scope_space_id":"space-1","discover":true,"send":false,"edit":false,` +
				`"expected_revision":0,"reason":"r","change_ticket":"t"}`},
		{name: "space principal with send", target: "/docs.access-request/grants/space/space-1",
			body: `{"scope_space_id":"","discover":true,"send":true,"edit":false,` +
				`"expected_revision":0,"reason":"r","change_ticket":"t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGrantCatalogStore{}
			api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
			response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut, tc.target, tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if store.upsertCalls != 0 {
				t.Fatal("non-canonical identity reached store")
			}
		})
	}
}

func TestGrantPrincipalExistenceFailsClosed(t *testing.T) {
	store := &fakeGrantCatalogStore{}
	principalErr := ErrGrantRequestInvalid
	api := &API{store: store, controlEnabled: true,
		validateGrantPrincipal: func(context.Context, GrantIdentity) error { return principalErr }}
	response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/docs.access-request/grants/bot/ghost-bot", grantUpsertBody())
	assertCatalogError(t, response, "err.server.card_template_catalog.grant_invalid", http.StatusBadRequest)
	if store.upsertCalls != 0 {
		t.Fatal("unknown principal reached store")
	}

	// A missing validator is a wiring bug and must fail closed, not open.
	api = &API{store: store, controlEnabled: true}
	response = doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/docs.access-request/grants/bot/bot-1", grantUpsertBody())
	assertCatalogError(t, response, "err.server.card_template_catalog.unavailable", http.StatusServiceUnavailable)
	if store.upsertCalls != 0 {
		t.Fatal("request without principal validator reached store")
	}
}

func TestGrantStoreErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		upsertErr  error
		wantCodeID string
		wantStatus int
	}{
		{name: "revision conflict", upsertErr: ErrGrantConflict,
			wantCodeID: "err.server.card_template_catalog.state_conflict", wantStatus: http.StatusConflict},
		{name: "target without claim", upsertErr: ErrGrantTargetUnknown,
			wantCodeID: "err.server.card_template_catalog.grant_invalid", wantStatus: http.StatusBadRequest},
		{name: "integrity", upsertErr: ErrCatalogIntegrity,
			wantCodeID: "err.shared.internal", wantStatus: http.StatusInternalServerError},
		{name: "invalid", upsertErr: ErrGrantRequestInvalid,
			wantCodeID: "err.server.card_template_catalog.grant_invalid", wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeGrantCatalogStore{upsertErr: tc.upsertErr}
			api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
			response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
				"/docs.access-request/grants/bot/bot-1", grantUpsertBody())
			assertCatalogError(t, response, tc.wantCodeID, tc.wantStatus)
		})
	}
}

func TestGrantRevokeFlow(t *testing.T) {
	store := &fakeGrantCatalogStore{revokeResult: GrantResult{
		Record: GrantRecord{
			Identity: GrantIdentity{
				TemplateID: "docs.access-request", PrincipalType: GrantPrincipalInternalProducer,
				PrincipalID: "docs-notify", ScopeSpaceID: "",
			},
			Status: GrantStatusRevoked, Revision: 4,
		},
	}}
	api := &API{store: store, controlEnabled: true, validateGrantPrincipal: allowAllGrantPrincipals}
	response := doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodDelete,
		"/docs.access-request/grants/internal_producer/docs-notify",
		`{"scope_space_id":"","expected_revision":3,"reason":"offboard","change_ticket":"CHG-11"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.revokeCalls != 1 || store.lastRevoke.ExpectedRevision != 3 || store.lastRevoke.ActorUID != "admin-1" {
		t.Fatalf("revoke request = %+v calls=%d", store.lastRevoke, store.revokeCalls)
	}
	body := response.Body.String()
	for _, needle := range []string{`"revision":4`, `"status":"revoked"`} {
		if !bytes.Contains([]byte(body), []byte(needle)) {
			t.Fatalf("response missing %s: %s", needle, body)
		}
	}

	// Already-revoked and not-found map to distinct manager-facing outcomes.
	store.revokeErr = ErrGrantAlreadyRevoked
	response = doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodDelete,
		"/docs.access-request/grants/internal_producer/docs-notify",
		`{"scope_space_id":"","expected_revision":4,"reason":"again","change_ticket":"CHG-12"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.state_conflict", http.StatusConflict)

	store.revokeErr = ErrGrantNotFound
	response = doGrantRequest(t, api, wkhttp.SuperAdmin, http.MethodDelete,
		"/docs.access-request/grants/internal_producer/ghost",
		`{"scope_space_id":"","expected_revision":0,"reason":"noop","change_ticket":"CHG-13"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.grant_invalid", http.StatusBadRequest)
}
