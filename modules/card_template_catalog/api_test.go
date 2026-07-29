package card_template_catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestValidateCompilesWithoutPersisting(t *testing.T) {
	store := &fakeCatalogStore{}
	compilerCalls := 0
	api := &API{
		store: store,
		compile: func(_ context.Context, bundle cardtmpl.Bundle, _ cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			compilerCalls++
			if bundle.Catalog.Engine != cardtmpl.JSONTemplateEngineV1 {
				t.Fatalf("compiler bundle engine = %q", bundle.Catalog.Engine)
			}
			return compiledArtifactFixture(), nil
		},
	}

	response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/validate", validControlRequestJSON(t, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if compilerCalls != 1 || store.publishCalls != 0 {
		t.Fatalf("compiler calls=%d publish calls=%d", compilerCalls, store.publishCalls)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["hash"] != strings.Repeat("a", 64) || body["active"] != false || body["published"] != false {
		t.Fatalf("validate response = %v", body)
	}
}

func TestPublishUsesAuthenticatedActorAndKeepsArtifactInactive(t *testing.T) {
	store := &fakeCatalogStore{publishResult: PublishResult{Created: true}}
	api := &API{
		store: store,
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			return compiledArtifactFixture(), nil
		},
	}

	response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/publish", validControlRequestJSON(t, true))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if store.publishCalls != 1 {
		t.Fatalf("publish calls = %d, want 1", store.publishCalls)
	}
	if store.lastPublish.ActorUID != "admin-1" || store.lastPublish.Reason != "reviewed" || store.lastPublish.ChangeTicket != "CHG-1" {
		t.Fatalf("publish request = %+v", store.lastPublish)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["active"] != false || body["created"] != true {
		t.Fatalf("publish response = %v", body)
	}
}

func TestPublishBoundsStoreContext(t *testing.T) {
	store := &fakeCatalogStore{publishResult: PublishResult{Created: true}}
	api := &API{
		store: store,
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			return compiledArtifactFixture(), nil
		},
	}

	response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/publish", validControlRequestJSON(t, true))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if store.publishContext == nil {
		t.Fatal("store did not receive a publish context")
	}
	if _, ok := store.publishContext.Deadline(); !ok {
		t.Fatal("store publish context has no server-side deadline")
	}
}

func TestControlEndpointsRejectNonSuperAdminBeforeCompile(t *testing.T) {
	compilerCalls := 0
	api := &API{
		store: &fakeCatalogStore{},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			compilerCalls++
			return compiledArtifactFixture(), nil
		},
	}
	for _, endpoint := range []string{"/validate", "/publish"} {
		response := doCatalogRequest(t, api, wkhttp.Admin, endpoint, validControlRequestJSON(t, endpoint == "/publish"))
		assertCatalogError(t, response, "err.server.card_template_catalog.forbidden", http.StatusForbidden)
	}
	if compilerCalls != 0 {
		t.Fatalf("compiler called %d times for forbidden requests", compilerCalls)
	}
}

func TestControlEndpointsRejectOversizeAndAmbiguousJSONBeforeCompile(t *testing.T) {
	compilerCalls := 0
	api := &API{
		store: &fakeCatalogStore{},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			compilerCalls++
			return compiledArtifactFixture(), nil
		},
	}

	t.Run("body limit", func(t *testing.T) {
		response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/validate", []byte(strings.Repeat("x", controlBodyMaxBytes+1)))
		assertCatalogError(t, response, "err.server.card_template_catalog.content_too_large", http.StatusRequestEntityTooLarge)
	})
	t.Run("duplicate wrapper key", func(t *testing.T) {
		valid := validControlRequestJSON(t, false)
		var request map[string]json.RawMessage
		if err := json.Unmarshal(valid, &request); err != nil {
			t.Fatal(err)
		}
		ambiguous := append([]byte(`{"bundle":`), request["bundle"]...)
		ambiguous = append(ambiguous, []byte(`,"bundle":`)...)
		ambiguous = append(ambiguous, request["bundle"]...)
		ambiguous = append(ambiguous, '}')
		response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/validate", ambiguous)
		assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	})
	if compilerCalls != 0 {
		t.Fatalf("compiler called %d times for rejected bodies", compilerCalls)
	}
}

func TestPublishRejectsCallerSuppliedActor(t *testing.T) {
	api := &API{store: &fakeCatalogStore{}, compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
		return compiledArtifactFixture(), nil
	}}
	var request map[string]any
	if err := json.Unmarshal(validControlRequestJSON(t, true), &request); err != nil {
		t.Fatal(err)
	}
	request["actor_uid"] = "spoofed-admin"
	body, _ := json.Marshal(request)
	response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/publish", body)
	assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
}

func TestPublishMapsConflictAndInternalErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
		wantDB     string
	}{
		{name: "immutable conflict", err: ErrImmutableVersionConflict, wantCode: "err.server.card_template_catalog.conflict", wantStatus: http.StatusConflict, wantDB: "conflict"},
		{name: "version claim conflict", err: ErrVersionClaimConflict, wantCode: "err.server.card_template_catalog.conflict", wantStatus: http.StatusConflict, wantDB: "conflict"},
		{name: "catalog integrity failure", err: ErrCatalogIntegrity, wantCode: "err.shared.internal", wantStatus: http.StatusInternalServerError, wantDB: "integrity_error"},
		{name: "store timeout", err: context.DeadlineExceeded, wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable, wantDB: "timeout"},
		{name: "store unavailable", err: errors.New("db unavailable"), wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable, wantDB: "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			api := &API{
				store:   &fakeCatalogStore{publishErr: tt.err},
				metrics: newCatalogMetrics(registry),
				compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
					return compiledArtifactFixture(), nil
				},
			}
			response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/publish", validControlRequestJSON(t, true))
			assertCatalogError(t, response, tt.wantCode, tt.wantStatus)
			if got := testutil.ToFloat64(api.metrics.db.WithLabelValues("publish", tt.wantDB)); got != 1 {
				t.Fatalf("publish DB metric result %q = %v, want 1", tt.wantDB, got)
			}
		})
	}
}

func TestReconcileStaticInventoryUsesFrozenRegistryList(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	registry.Freeze()
	store := &fakeCatalogStore{}
	if err := reconcileStaticInventory(context.Background(), store, registry); err != nil {
		t.Fatalf("reconcileStaticInventory: %v", err)
	}
	if len(store.reconciled) != 1 || store.reconciled[0].ID != aireasoningprocess.TemplateID || store.reconciled[0].Version != aireasoningprocess.TemplateVersionV2 {
		t.Fatalf("reconciled inventory = %+v", store.reconciled)
	}
}

func TestCatalogNoLegacyErrorResponses(t *testing.T) {
	files := []string{"api.go", "api_i18n.go"}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(", ".AbortWithStatusJSON(", ".AbortWithStatus(", `.Response("`}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		cleaned := stripLineComments(string(data))
		for _, token := range banned {
			if strings.Contains(cleaned, token) {
				t.Fatalf("%s contains legacy error response %s", file, token)
			}
		}
	}
}

func doCatalogRequest(t *testing.T, api *API, role wkhttp.UserRole, endpoint string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	route.Use(func(c *wkhttp.Context) {
		c.Set("uid", "admin-1")
		c.Set("role", string(role))
	})
	route.POST("/validate", api.validate)
	route.POST("/publish", api.publish)
	request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, request)
	return recorder
}

func validControlRequestJSON(t *testing.T, publish bool) []byte {
	t.Helper()
	bundle := cardtmpl.Bundle{
		Catalog: cardtmpl.CatalogDescriptor{Engine: cardtmpl.JSONTemplateEngineV1, Visibility: cardtmpl.CatalogVisibilityPrivate},
		Manifest: json.RawMessage(`{
			"schemaVersion":2,"id":"test.runtime-card","name":"Runtime card","version":"1.0.0",
			"contractVersion":"1.0.0","protocol":"octo-card@1.0","adaptiveCardVersion":"1.5","owner":"ai",
			"dataSchema":"contract/data.schema.json",
			"views":{"main":{"wireProfile":"octo/v1","states":["shown"],"template":"templates/main.template.json","samples":["samples/shown.json"]}}
		}`),
		Schema: json.RawMessage(`{
			"$schema":"http://json-schema.org/draft-07/schema#","type":"object","additionalProperties":false,
			"required":["title"],"properties":{"title":{"type":"string","maxLength":32}}
		}`),
		Templates: map[string]json.RawMessage{
			"main": json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"${title}"}]}`),
		},
		Samples: map[string]json.RawMessage{"shown": json.RawMessage(`{"title":"hello"}`)},
	}
	request := map[string]any{"bundle": bundle}
	if publish {
		request["reason"] = "reviewed"
		request["change_ticket"] = "CHG-1"
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal control request: %v", err)
	}
	return body
}

func compiledArtifactFixture() *cardtmpl.CompiledArtifact {
	return &cardtmpl.CompiledArtifact{
		Meta:            cardtmpl.TemplateMeta{ID: "test.runtime-card", Version: "1.0.0", Protocol: cardtmpl.Protocol},
		Hash:            strings.Repeat("a", 64),
		Bundle:          cardtmpl.CanonicalBundle(`{"canonical":true}`),
		Engine:          cardtmpl.JSONTemplateEngineV1,
		Visibility:      cardtmpl.CatalogVisibilityPrivate,
		Owner:           "ai",
		ContractVersion: "1.0.0",
	}
}

type fakeCatalogStore struct {
	publishResult  PublishResult
	publishErr     error
	publishCalls   int
	publishContext context.Context
	lastPublish    PublishRequest
	reconciled     []cardtmpl.TemplateMeta
	reconcileErr   error
	reconcileErrs  []error
	reconcileCalls int
}

func (s *fakeCatalogStore) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	s.publishCalls++
	s.publishContext = ctx
	s.lastPublish = request
	return s.publishResult, s.publishErr
}

func (s *fakeCatalogStore) ReconcileStatic(_ context.Context, inventory []cardtmpl.TemplateMeta) error {
	s.reconcileCalls++
	s.reconciled = append([]cardtmpl.TemplateMeta(nil), inventory...)
	if len(s.reconcileErrs) > 0 {
		err := s.reconcileErrs[0]
		s.reconcileErrs = s.reconcileErrs[1:]
		return err
	}
	return s.reconcileErr
}

type catalogErrorEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func assertCatalogError(t *testing.T, response *httptest.ResponseRecorder, wantCode string, wantStatus int) {
	t.Helper()
	if response.Code != http.StatusBadRequest {
		t.Fatalf("transport status = %d, want D14 400; body=%s", response.Code, response.Body.String())
	}
	var envelope catalogErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v body=%s", err, response.Body.String())
	}
	if envelope.Error.Code != wantCode || envelope.Error.HTTPStatus != wantStatus {
		t.Fatalf("error = %+v, want code=%s status=%d", envelope.Error, wantCode, wantStatus)
	}
}

func stripLineComments(source string) string {
	var cleaned strings.Builder
	for _, line := range strings.Split(source, "\n") {
		if index := strings.Index(line, "//"); index >= 0 {
			line = line[:index]
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	return cleaned.String()
}
