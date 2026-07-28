package card_template_catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewInTestModeAllowsUninstalledStaticRegistry(t *testing.T) {
	previous := cardtmpl.DefaultRegistry()
	cardtmpl.SetDefaultRegistry(nil)
	t.Cleanup(func() { cardtmpl.SetDefaultRegistry(previous) })

	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	api := New(ctx)
	if api.ctx != ctx || api.store == nil || api.compile == nil || api.logger == nil {
		t.Fatalf("New returned incomplete API: %+v", api)
	}
}

func TestRouteRequiresAuthenticationBeforeControlHandlers(t *testing.T) {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	compilerCalls := 0
	api := &API{
		ctx:   ctx,
		store: &fakeCatalogStore{},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			compilerCalls++
			return compiledArtifactFixture(), nil
		},
	}
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	api.Route(route)

	request := httptest.NewRequest(http.MethodPost, "/v1/manager/card-templates/validate", nil)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", response.Code, response.Body.String())
	}
	if compilerCalls != 0 {
		t.Fatalf("compiler called %d times before auth", compilerCalls)
	}
}

func TestCompileRequestMapsValidationAndCapacityErrors(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantOperation string
	}{
		{
			name:          "validation",
			err:           &cardtmpl.ArtifactValidationError{Category: "schema", Document: "schema"},
			wantCode:      "err.server.card_template_catalog.request_invalid",
			wantOperation: "rejected",
		},
		{
			name:          "capacity",
			err:           cardtmpl.ErrArtifactCompileBusy,
			wantCode:      "err.server.card_template_catalog.unavailable",
			wantOperation: "busy",
		},
		{
			name:          "timeout",
			err:           context.DeadlineExceeded,
			wantCode:      "err.server.card_template_catalog.unavailable",
			wantOperation: "timeout",
		},
		{
			name:          "internal",
			err:           errors.New("compiler failed"),
			wantCode:      "err.server.card_template_catalog.unavailable",
			wantOperation: "error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			api := &API{
				store:   &fakeCatalogStore{},
				metrics: newCatalogMetrics(registry),
				compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
					return nil, tt.err
				},
			}
			response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/validate", validControlRequestJSON(t, false))
			var envelope catalogErrorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Error.Code, tt.wantCode)
			}
			if got := testutil.ToFloat64(api.metrics.operations.WithLabelValues("validate", tt.wantOperation)); got != 1 {
				t.Fatalf("validate operation result %q = %v, want 1", tt.wantOperation, got)
			}
		})
	}
}

func TestPublishRejectsMissingAuditFieldsBeforeCompile(t *testing.T) {
	compilerCalls := 0
	api := &API{
		store: &fakeCatalogStore{},
		compile: func(context.Context, cardtmpl.Bundle, cardtmpl.CompileLimits) (*cardtmpl.CompiledArtifact, error) {
			compilerCalls++
			return compiledArtifactFixture(), nil
		},
	}
	response := doCatalogRequest(t, api, wkhttp.SuperAdmin, "/publish", validControlRequestJSON(t, false))
	assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if compilerCalls != 0 {
		t.Fatalf("compiler called %d times before audit-field validation", compilerCalls)
	}
}

func TestReconcileStaticInventoryRejectsMissingDependencies(t *testing.T) {
	if err := reconcileStaticInventory(context.Background(), nil, nil); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("error = %v, want ErrCatalogIntegrity", err)
	}
}

func TestReconcileStaticInventoryRetriesTransientFailure(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	store := &fakeCatalogStore{reconcileErrs: []error{errors.New("deadlock"), nil}}
	if err := reconcileStaticInventory(context.Background(), store, registry); err != nil {
		t.Fatalf("reconcileStaticInventory: %v", err)
	}
	if store.reconcileCalls != 2 {
		t.Fatalf("reconcile calls = %d, want 2", store.reconcileCalls)
	}
}

func TestReconcileStaticInventoryDoesNotRetryIntegrityFailure(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	store := &fakeCatalogStore{reconcileErrs: []error{ErrCatalogIntegrity}}
	if err := reconcileStaticInventory(context.Background(), store, registry); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("reconcileStaticInventory error = %v, want ErrCatalogIntegrity", err)
	}
	if store.reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", store.reconcileCalls)
	}
}
