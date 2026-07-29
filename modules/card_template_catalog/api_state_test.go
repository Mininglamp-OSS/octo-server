package card_template_catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/prometheus/client_golang/prometheus"
)

func TestActivateGateDefaultsClosedAndDoesNotTouchStore(t *testing.T) {
	store := &fakeStateCatalogStore{}
	api := &API{store: store, controlEnabled: false}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":0,"reason":"rollout","change_ticket":"CHG-1"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.control_disabled", http.StatusServiceUnavailable)
	if store.activateCalls != 0 {
		t.Fatalf("activate calls = %d, want 0", store.activateCalls)
	}
}

func TestRuntimeReadinessRejectsStateWritesBeforeTargetValidation(t *testing.T) {
	store := &fakeStateCatalogStore{}
	validateCalls := 0
	api := &API{
		store: store, controlEnabled: true, readiness: newRuntimeCatalogReadiness(),
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			validateCalls++
			return stateTargetReceipt{}, nil
		},
	}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":0,"reason":"rollout","change_ticket":"CHG-1"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.unavailable", http.StatusServiceUnavailable)
	if validateCalls != 0 || store.activateCalls != 0 {
		t.Fatalf("validation/store calls = %d/%d, want 0/0", validateCalls, store.activateCalls)
	}
}

func TestActivateValidatesTargetAndUsesAuthenticatedActor(t *testing.T) {
	store := &fakeStateCatalogStore{activateResult: ActivationResult{
		TemplateID: "test.runtime-card", ActiveVersion: "1.0.0", Status: activationStatusActive, Revision: 1,
	}}
	validated := ""
	api := &API{
		store: store, controlEnabled: true,
		validateStateTarget: func(_ context.Context, id cardtmpl.ID, version string) (stateTargetReceipt, error) {
			validated = string(id) + "@" + version
			return dynamicTargetReceiptFixture(id, version, strings.Repeat("a", 64)), nil
		},
	}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":0,"reason":"rollout","change_ticket":"CHG-1"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if validated != "test.runtime-card@1.0.0" || store.activateCalls != 1 || store.lastActivate.ActorUID != "admin-1" {
		t.Fatalf("validated=%q calls=%d request=%+v", validated, store.activateCalls, store.lastActivate)
	}
}

func TestRollbackAndBlockRemainAvailableWhenForwardControlDisabled(t *testing.T) {
	store := &fakeStateCatalogStore{
		rollbackResult: ActivationResult{
			TemplateID: "test.runtime-card", ActiveVersion: "1.0.0", Status: activationStatusActive, Revision: 5,
		},
		blockResult: BlockResult{
			TemplateID: "test.runtime-card", Version: "1.1.0", Blocked: true,
			Status: activationStatusDisabled, Revision: 6,
		},
	}
	api := &API{
		store: store, controlEnabled: false,
		validateStateTarget: func(_ context.Context, id cardtmpl.ID, version string) (stateTargetReceipt, error) {
			return dynamicTargetReceiptFixture(id, version, strings.Repeat("a", 64)), nil
		},
	}
	rollback := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/test.runtime-card/rollback", `{"target_version":"1.0.0","expected_revision":4,"reason":"incident","change_ticket":"INC-1"}`)
	if rollback.Code != http.StatusOK || store.rollbackCalls != 1 {
		t.Fatalf("rollback status=%d calls=%d body=%s", rollback.Code, store.rollbackCalls, rollback.Body.String())
	}
	block := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/test.runtime-card@1.1.0/block", `{"expected_revision":5,"reason":"security","change_ticket":"SEC-1"}`)
	if block.Code != http.StatusOK || store.blockCalls != 1 {
		t.Fatalf("block status=%d calls=%d body=%s", block.Code, store.blockCalls, block.Body.String())
	}
}

func TestStateWritesRejectUnknownFieldsBeforeValidation(t *testing.T) {
	store := &fakeStateCatalogStore{}
	validateCalls := 0
	api := &API{
		store: store, controlEnabled: true,
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			validateCalls++
			return stateTargetReceipt{}, nil
		},
	}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":0,"reason":"rollout","change_ticket":"CHG-1","actor_uid":"spoof"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if validateCalls != 0 || store.activateCalls != 0 {
		t.Fatalf("validation/store calls = %d/%d, want 0/0", validateCalls, store.activateCalls)
	}
}

func TestStateWritesRejectNullExpectedRevisionBeforeValidation(t *testing.T) {
	store := &fakeStateCatalogStore{}
	validateCalls := 0
	api := &API{
		store: store, controlEnabled: true,
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			validateCalls++
			return stateTargetReceipt{}, nil
		},
	}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
		"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":null,"reason":"rollout","change_ticket":"CHG-1"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if validateCalls != 0 || store.activateCalls != 0 {
		t.Fatalf("validation/store calls = %d/%d, want 0/0", validateCalls, store.activateCalls)
	}
}

func TestBlockRejectsNullOptionalFallbackBeforeStore(t *testing.T) {
	store := &fakeStateCatalogStore{}
	api := &API{store: store}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/test.runtime-card@1.0.0/block", `{"expected_revision":1,"fallback_version":null,"reason":"incident","change_ticket":"INC-1"}`)
	assertCatalogError(t, response, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if store.blockCalls != 0 {
		t.Fatalf("block store calls = %d, want 0", store.blockCalls)
	}
}

func TestManagerDetailAndAuditAreBoundedAndOmitBundle(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	store := &fakeStateCatalogStore{
		detail: TemplateDetail{
			TemplateID: "test.runtime-card",
			Activation: cardtmpl.RuntimeActivation{Exists: true, Version: "1.0.0", Status: cardtmpl.RuntimeActivationActive, Revision: 1},
			Versions:   []TemplateVersionDetail{{Version: "1.0.0", Source: cardtmpl.RuntimeSourceDynamic, Hash: "abc", CreatedAt: now}},
		},
		auditPage: TemplateAuditPage{Entries: []TemplateAuditEntry{{
			ID: 9, ActorUID: "admin-1", Operation: "activate", Version: "1.0.0", Result: "ok", CreatedAt: now,
		}}},
	}
	api := &API{store: store}
	detail := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card", "")
	if detail.Code != http.StatusOK || bytes.Contains(detail.Body.Bytes(), []byte("canonical_bundle")) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	audit := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card/audit?limit=1000", "")
	if audit.Code != http.StatusOK || store.lastAuditLimit != managerReadHardLimit {
		t.Fatalf("audit status=%d limit=%d body=%s", audit.Code, store.lastAuditLimit, audit.Body.String())
	}
}

func TestManagerDetailAndAuditRemainAvailableWhileRuntimeIsNotReady(t *testing.T) {
	tests := []struct {
		name      string
		readiness *runtimeCatalogReadiness
	}{
		{name: "startup pending", readiness: newRuntimeCatalogReadiness()},
		{name: "integrity failure", readiness: integrityReadinessFixture()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStateCatalogStore{
				detail:    TemplateDetail{TemplateID: "test.runtime-card"},
				auditPage: TemplateAuditPage{},
			}
			api := &API{store: store, readiness: test.readiness}
			detail := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card", "")
			if detail.Code != http.StatusOK {
				t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
			}
			audit := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card/audit", "")
			if audit.Code != http.StatusOK {
				t.Fatalf("audit status=%d body=%s", audit.Code, audit.Body.String())
			}
		})
	}
}

func integrityReadinessFixture() *runtimeCatalogReadiness {
	readiness := newRuntimeCatalogReadiness()
	readiness.MarkIntegrity(errors.New("stored activation is invalid"))
	return readiness
}

func TestStateErrorsUseStableLocalizedMappings(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   string
		wantStatus int
	}{
		{name: "revision conflict", err: ErrActivationConflict, wantCode: "err.server.card_template_catalog.state_conflict", wantStatus: http.StatusConflict},
		{name: "capacity conflict", err: ErrActiveTargetCapacity, wantCode: "err.server.card_template_catalog.state_conflict", wantStatus: http.StatusConflict},
		{name: "blocked", err: ErrArtifactBlocked, wantCode: "err.server.card_template_catalog.blocked", wantStatus: http.StatusConflict},
		{name: "invalid target", err: ErrActivationTargetInvalid, wantCode: "err.server.card_template_catalog.request_invalid", wantStatus: http.StatusBadRequest},
		{name: "integrity", err: ErrCatalogIntegrity, wantCode: "err.shared.internal", wantStatus: http.StatusInternalServerError},
		{name: "canceled", err: context.Canceled, wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable},
		{name: "compile busy", err: cardtmpl.ErrArtifactCompileBusy, wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable},
		{name: "database", err: errors.New("db unavailable"), wantCode: "err.server.card_template_catalog.unavailable", wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStateCatalogStore{activateErr: test.err}
			api := &API{
				store: store, controlEnabled: true, metrics: newCatalogMetrics(prometheus.NewRegistry()),
				validateStateTarget: func(_ context.Context, id cardtmpl.ID, version string) (stateTargetReceipt, error) {
					return dynamicTargetReceiptFixture(id, version, strings.Repeat("a", 64)), nil
				},
			}
			response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPut,
				"/test.runtime-card/active", `{"version":"1.0.0","expected_revision":0,"reason":"rollout","change_ticket":"CHG-1"}`)
			assertCatalogError(t, response, test.wantCode, test.wantStatus)
		})
	}
}

func TestStateStoreUnavailableFailsClosed(t *testing.T) {
	api := &API{store: &fakeCatalogStore{}}
	response := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card", "")
	assertCatalogError(t, response, "err.server.card_template_catalog.unavailable", http.StatusServiceUnavailable)
}

func TestManagerReadErrorsAndPaginationValidationFailClosed(t *testing.T) {
	store := &fakeStateCatalogStore{detailErr: errors.New("db unavailable"), auditErr: errors.New("db unavailable")}
	api := &API{store: store, metrics: newCatalogMetrics(prometheus.NewRegistry())}
	detail := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card", "")
	assertCatalogError(t, detail, "err.server.card_template_catalog.unavailable", http.StatusServiceUnavailable)
	audit := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card/audit", "")
	assertCatalogError(t, audit, "err.server.card_template_catalog.unavailable", http.StatusServiceUnavailable)
	invalidCursor := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet, "/test.runtime-card/audit?cursor=-1", "")
	assertCatalogError(t, invalidCursor, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if got := normalizeManagerReadLimit(0); got != managerReadDefaultLimit {
		t.Fatalf("default normalized limit=%d", got)
	}
	if got := normalizeManagerReadLimit(managerReadHardLimit + 1); got != managerReadHardLimit {
		t.Fatalf("hard normalized limit=%d", got)
	}
}

func TestStateHandlersRejectInvalidTargetsBeforeStore(t *testing.T) {
	store := &fakeStateCatalogStore{}
	api := &API{
		store: store, controlEnabled: true,
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			return stateTargetReceipt{}, ErrActivationTargetInvalid
		},
	}
	rollback := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/test.runtime-card/rollback", `{"target_version":"1.0.0","expected_revision":1,"reason":"incident","change_ticket":"INC-1"}`)
	assertCatalogError(t, rollback, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	block := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/test.runtime-card@1.1.0/block", `{"expected_revision":1,"fallback_version":"1.0.0","reason":"incident","change_ticket":"INC-1"}`)
	assertCatalogError(t, block, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	badIdentity := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodPost,
		"/INVALID/block", `{"expected_revision":1,"reason":"incident","change_ticket":"INC-1"}`)
	assertCatalogError(t, badIdentity, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	badLimit := doStateRequest(t, api, wkhttp.SuperAdmin, http.MethodGet,
		"/test.runtime-card/audit?limit=invalid", "")
	assertCatalogError(t, badLimit, "err.server.card_template_catalog.request_invalid", http.StatusBadRequest)
	if store.rollbackCalls != 0 || store.blockCalls != 0 {
		t.Fatalf("rollback/block store calls = %d/%d, want 0/0", store.rollbackCalls, store.blockCalls)
	}
}

func doStateRequest(
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
	group.GET("/:id/audit", api.audit)
	group.GET("/:id", api.detail)
	group.PUT("/:id/active", api.activate)
	group.POST("/:id/rollback", api.rollback)
	group.POST("/:id/block", api.block)
	request := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, request)
	return recorder
}

type fakeStateCatalogStore struct {
	fakeCatalogStore
	activateResult ActivationResult
	activateErr    error
	activateCalls  int
	lastActivate   ActivationRequest
	rollbackResult ActivationResult
	rollbackErr    error
	rollbackCalls  int
	lastRollback   RollbackRequest
	blockResult    BlockResult
	blockErr       error
	blockCalls     int
	lastBlock      BlockRequest
	detail         TemplateDetail
	detailErr      error
	auditPage      TemplateAuditPage
	auditErr       error
	lastAuditLimit int
}

func (s *fakeStateCatalogStore) Activate(_ context.Context, request ActivationRequest) (ActivationResult, error) {
	s.activateCalls++
	s.lastActivate = request
	return s.activateResult, s.activateErr
}

func (s *fakeStateCatalogStore) Rollback(_ context.Context, request RollbackRequest) (ActivationResult, error) {
	s.rollbackCalls++
	s.lastRollback = request
	return s.rollbackResult, s.rollbackErr
}

func (s *fakeStateCatalogStore) Block(_ context.Context, request BlockRequest) (BlockResult, error) {
	s.blockCalls++
	s.lastBlock = request
	return s.blockResult, s.blockErr
}

func (s *fakeStateCatalogStore) GetTemplateDetail(context.Context, cardtmpl.ID, int) (TemplateDetail, error) {
	return s.detail, s.detailErr
}

func (s *fakeStateCatalogStore) ListAudit(_ context.Context, _ cardtmpl.ID, _ uint64, limit int) (TemplateAuditPage, error) {
	s.lastAuditLimit = limit
	return s.auditPage, s.auditErr
}

func decodeStateResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
