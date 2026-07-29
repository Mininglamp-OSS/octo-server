package card_template_catalog

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCatalogMetricsExposeOnlyBoundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newCatalogMetrics(registry)
	again := newCatalogMetrics(registry)
	if again.operations != metrics.operations || again.compile != metrics.compile || again.db != metrics.db {
		t.Fatal("duplicate registration must reuse the existing collectors")
	}
	metrics.observeOperation("validate", "ok")
	metrics.observeCompile("validation_error", 25*time.Millisecond)
	metrics.observeDB("publish", "conflict")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	want := map[string]bool{
		"dmwork_card_catalog_operation_total": false,
		"dmwork_card_catalog_compile_seconds": false,
		"dmwork_card_catalog_db_total":        false,
	}
	for _, family := range families {
		name := family.GetName()
		if _, ok := want[name]; !ok {
			continue
		}
		want[name] = true
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if !allowedCatalogMetricLabel(name, label) {
					t.Errorf("metric %s has unexpected/high-cardinality label %s", name, label.GetName())
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not registered", name)
		}
	}
}

func allowedCatalogMetricLabel(metric string, label *dto.LabelPair) bool {
	switch metric {
	case "dmwork_card_catalog_compile_seconds":
		return label.GetName() == "result"
	case "dmwork_card_catalog_operation_total", "dmwork_card_catalog_db_total":
		return label.GetName() == "operation" || label.GetName() == "result"
	default:
		return false
	}
}
