package card_template_catalog

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRuntimeCatalogReadinessDistinguishesPendingReadyAndIntegrity(t *testing.T) {
	readiness := newRuntimeCatalogReadiness()
	if err := readiness.Check(); !errors.Is(err, cardtmpl.ErrRuntimeCatalogUnavailable) {
		t.Fatalf("pending readiness error = %v, want ErrRuntimeCatalogUnavailable", err)
	}
	readiness.MarkReady()
	if err := readiness.Check(); err != nil {
		t.Fatalf("ready check: %v", err)
	}

	poisoned := newRuntimeCatalogReadiness()
	poisoned.MarkIntegrity(errors.New("source conflict"))
	if err := poisoned.Check(); !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("poisoned readiness error = %v, want ErrRuntimeCatalogIntegrity", err)
	}
	poisoned.MarkReady()
	if err := poisoned.Check(); !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("integrity readiness was reopened: %v", err)
	}
}

func TestInitializeRuntimeCatalogReconcilesBeforeValidatingActiveTargets(t *testing.T) {
	store := &startupStoreStub{targets: []startupActivationTarget{
		{ID: "test.runtime-a", Version: "1.0.0"},
		{ID: "test.runtime-b", Version: "2.0.0"},
	}}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	api := &API{
		store: store, registry: registry,
		validateStateTarget: func(_ context.Context, id cardtmpl.ID, version string) (stateTargetReceipt, error) {
			store.calls = append(store.calls, "validate:"+string(id)+"@"+version)
			return stateTargetReceipt{}, nil
		},
	}
	if err := api.initializeRuntimeCatalog(context.Background()); err != nil {
		t.Fatalf("initializeRuntimeCatalog: %v", err)
	}
	want := []string{"reconcile", "list", "validate:test.runtime-a@1.0.0", "validate:test.runtime-b@2.0.0"}
	if !reflect.DeepEqual(store.calls, want) {
		t.Fatalf("startup order = %v, want %v", store.calls, want)
	}
}

func TestInitializeRuntimeCatalogFailsClosedOnInvalidActiveTarget(t *testing.T) {
	store := &startupStoreStub{targets: []startupActivationTarget{{ID: "test.runtime-a", Version: "1.0.0"}}}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	api := &API{
		store: store, registry: registry,
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			return stateTargetReceipt{}, ErrActivationTargetInvalid
		},
	}
	err := api.initializeRuntimeCatalog(context.Background())
	if !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("startup validation error = %v, want ErrCatalogIntegrity", err)
	}
}

func TestInitializeRuntimeCatalogBoundsEachActiveTargetValidation(t *testing.T) {
	store := &startupStoreStub{targets: []startupActivationTarget{
		{ID: "test.runtime-a", Version: "1.0.0"},
		{ID: "test.runtime-b", Version: "2.0.0"},
	}}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	validated := 0
	api := &API{
		store: store, registry: registry,
		validateStateTarget: func(ctx context.Context, _ cardtmpl.ID, _ string) (stateTargetReceipt, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 15*time.Second {
				return stateTargetReceipt{}, errors.New("active target validation context is not independently bounded")
			}
			validated++
			return stateTargetReceipt{}, nil
		},
	}
	if err := api.initializeRuntimeCatalog(context.Background()); err != nil {
		t.Fatalf("initializeRuntimeCatalog: %v", err)
	}
	if validated != 2 {
		t.Fatalf("validated targets = %d, want 2", validated)
	}
}

func TestInitializeRuntimeCatalogReportsActiveTargetCountOnOverflow(t *testing.T) {
	targets := make([]startupActivationTarget, startupActiveTargetLimit+1)
	store := &startupStoreStub{
		targets: targets,
		listErr: ErrCatalogIntegrity,
	}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	metrics := newCatalogMetrics(prometheus.NewRegistry())
	api := &API{store: store, registry: registry, metrics: metrics}

	if err := api.initializeRuntimeCatalog(context.Background()); !errors.Is(err, ErrCatalogIntegrity) {
		t.Fatalf("initializeRuntimeCatalog overflow error = %v, want ErrCatalogIntegrity", err)
	}
	if got := testutil.ToFloat64(metrics.activeTargets); got != startupActiveTargetLimit+1 {
		t.Fatalf("active target gauge = %v, want %d", got, startupActiveTargetLimit+1)
	}
}

func TestInitializeRuntimeCatalogAcceptsRegisteredStaticReasoningTargets(t *testing.T) {
	t.Setenv("OCTO_CARD_ACTION_ROUTES", "")
	registry := cardtmpl.NewRegistry()
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV2)
	registry.RegisterJSON(aireasoningprocess.Assets, aireasoningprocess.HandoffRootV3)
	registry.Freeze()
	store := &startupStoreStub{targets: []startupActivationTarget{
		{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV2},
		{ID: aireasoningprocess.TemplateID, Version: aireasoningprocess.TemplateVersionV3},
	}}
	api := &API{store: store, registry: registry}
	api.validateStateTarget = api.validateTarget

	if err := api.initializeRuntimeCatalog(context.Background()); err != nil {
		t.Fatalf("initialize registered static reasoning targets: %v", err)
	}
}

func TestAPIStartDoesNotBlockOnDatabaseReconciliation(t *testing.T) {
	store := &blockingStartupStore{started: make(chan struct{})}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	api := &API{
		store: store, registry: registry, readiness: newRuntimeCatalogReadiness(),
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			return stateTargetReceipt{}, nil
		},
	}
	startResult := make(chan error, 1)
	go func() { startResult <- api.Start() }()
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start blocked on database reconciliation")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("background reconciliation did not start")
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- api.Stop() }()
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel background reconciliation")
	}
}

func TestAPIStartMarksReadyOnlyAfterSuccessfulInitialization(t *testing.T) {
	store := &lifecycleStartupStore{}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	readiness := newRuntimeCatalogReadiness()
	api := &API{
		store: store, registry: registry, readiness: readiness,
		validateStateTarget: func(context.Context, cardtmpl.ID, string) (stateTargetReceipt, error) {
			return stateTargetReceipt{}, nil
		},
	}
	if err := api.Start(); err != nil {
		t.Fatal(err)
	}
	waitForStartupDone(t, api)
	if err := readiness.Check(); err != nil {
		t.Fatalf("readiness after successful initialization: %v", err)
	}
	if err := api.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIStartPoisonsReadinessOnPersistentIntegrityFailure(t *testing.T) {
	store := &lifecycleStartupStore{fakeCatalogStore: fakeCatalogStore{reconcileErr: ErrCatalogIntegrity}}
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	readiness := newRuntimeCatalogReadiness()
	api := &API{store: store, registry: registry, readiness: readiness}
	if err := api.Start(); err != nil {
		t.Fatal(err)
	}
	waitForStartupDone(t, api)
	if err := readiness.Check(); !errors.Is(err, cardtmpl.ErrRuntimeCatalogIntegrity) {
		t.Fatalf("integrity readiness error = %v", err)
	}
}

func waitForStartupDone(t *testing.T, api *API) {
	t.Helper()
	api.startupMu.Lock()
	done := api.startupDone
	api.startupMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime catalog startup did not finish")
	}
}

type startupStoreStub struct {
	fakeCatalogStore
	targets []startupActivationTarget
	listErr error
	calls   []string
}

type blockingStartupStore struct {
	fakeCatalogStore
	started chan struct{}
	once    sync.Once
}

type lifecycleStartupStore struct {
	fakeCatalogStore
}

func (s *lifecycleStartupStore) ListActiveTargets(context.Context) ([]startupActivationTarget, error) {
	return nil, nil
}

func (s *blockingStartupStore) ReconcileStatic(ctx context.Context, _ []cardtmpl.TemplateMeta) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingStartupStore) ListActiveTargets(context.Context) ([]startupActivationTarget, error) {
	return nil, nil
}

func (s *startupStoreStub) ReconcileStatic(context.Context, []cardtmpl.TemplateMeta) error {
	s.calls = append(s.calls, "reconcile")
	return nil
}

func (s *startupStoreStub) ListActiveTargets(context.Context) ([]startupActivationTarget, error) {
	s.calls = append(s.calls, "list")
	return append([]startupActivationTarget(nil), s.targets...), s.listErr
}
