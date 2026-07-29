package card_template_catalog

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInstallRuntimeCatalogPublishesOneCatalogAndStore(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := cardtmpl.NewRegistry()
	registry.Freeze()

	previousStore := installedRuntimeStore()
	previousGates := installedRuntimeGates()
	previousReadiness := installedRuntimeReadiness()
	t.Cleanup(func() {
		cardtmpl.SetDefaultCatalog(nil)
		setInstalledRuntimeStore(previousStore)
		setInstalledRuntimeGates(previousGates)
		setInstalledRuntimeReadiness(previousReadiness)
	})

	catalog, err := InstallRuntimeCatalog(db, registry, cardtmpl.RuntimeCatalogConfig{})
	if err != nil {
		t.Fatalf("InstallRuntimeCatalog: %v", err)
	}
	if catalog == nil || cardtmpl.DefaultCatalog() != catalog {
		t.Fatal("installed catalog is not the process default")
	}
	if installedRuntimeStore() == nil {
		t.Fatal("runtime store was not shared with the module control plane")
	}
	readiness := installedRuntimeReadiness()
	if readiness == nil {
		t.Fatal("runtime readiness was not shared with the module control plane")
	}
	if err := readiness.Check(); err == nil {
		t.Fatal("runtime readiness must remain pending before startup reconciliation")
	}
	if err := catalog.CheckReady(); !errors.Is(err, cardtmpl.ErrRuntimeCatalogUnavailable) {
		t.Fatalf("default catalog pending readiness = %v, want ErrRuntimeCatalogUnavailable", err)
	}
	readiness.MarkReady()
	if err := readiness.Check(); err != nil {
		t.Fatalf("ready check: %v", err)
	}
	if err := catalog.CheckReady(); err != nil {
		t.Fatalf("default catalog ready check: %v", err)
	}
}

func TestInstallRuntimeCatalogRejectsMissingDependencies(t *testing.T) {
	registry := cardtmpl.NewRegistry()
	registry.Freeze()
	if _, err := InstallRuntimeCatalog(nil, registry, cardtmpl.RuntimeCatalogConfig{}); err == nil {
		t.Fatal("InstallRuntimeCatalog(nil DB) error = nil")
	}
}

func TestRuntimeMetricHooksFanOutAndUpdateBoundedMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newCatalogMetrics(registry)
	resolveCalls, cacheCalls, sizeCalls := 0, 0, 0
	hooks := runtimeMetricHooks(cardtmpl.RuntimeCatalogHooks{
		OnResolve: func(cardtmpl.RuntimeSource, string) { resolveCalls++ },
		OnCache:   func(string) { cacheCalls++ },
		OnCacheSize: func(int, int) {
			sizeCalls++
		},
	}, metrics)
	hooks.OnResolve(cardtmpl.RuntimeSourceDynamic, "ok")
	hooks.OnCache("hit")
	hooks.OnCacheSize(2, 1024)
	metrics.setActiveTargetCount(3)
	if resolveCalls != 1 || cacheCalls != 1 || sizeCalls != 1 {
		t.Fatalf("existing hook calls = %d/%d/%d", resolveCalls, cacheCalls, sizeCalls)
	}
	if got := testutil.ToFloat64(metrics.resolve.WithLabelValues("dynamic", "ok")); got != 1 {
		t.Fatalf("resolve metric = %v", got)
	}
	if got := testutil.ToFloat64(metrics.cache.WithLabelValues("hit")); got != 1 {
		t.Fatalf("cache metric = %v", got)
	}
	if got := testutil.ToFloat64(metrics.cacheEntries); got != 2 {
		t.Fatalf("cache entries gauge = %v", got)
	}
	if got := testutil.ToFloat64(metrics.cacheBytes); got != 1024 {
		t.Fatalf("cache bytes gauge = %v", got)
	}
	if got := testutil.ToFloat64(metrics.activeTargets); got != 3 {
		t.Fatalf("active target gauge = %v", got)
	}
}
