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
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "validation",
			err:      &cardtmpl.ArtifactValidationError{Category: "schema", Document: "schema"},
			wantCode: "err.server.card_template_catalog.request_invalid",
		},
		{
			name:     "capacity",
			err:      cardtmpl.ErrArtifactCompileBusy,
			wantCode: "err.server.card_template_catalog.unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &API{
				store: &fakeCatalogStore{},
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
