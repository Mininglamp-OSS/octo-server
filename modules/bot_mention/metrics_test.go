package bot_mention

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestBotMentionMetricsRegisterLowCardinalityResults(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotMentionMetrics(registry)

	metrics.ObserveIngress("accepted", "ppt", 25*time.Millisecond)
	metrics.ObserveIngress("not_found", "html", 20*time.Millisecond)
	metrics.ObserveIngress("unexpected-result", "untrusted-doc-kind", 10*time.Millisecond)
	metrics.ObserveEnqueue("accepted", 15*time.Millisecond)
	metrics.ObserveEnqueue("unexpected-result", 5*time.Millisecond)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := make(map[string]map[string]bool)
	for _, family := range families {
		results := make(map[string]bool)
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "result" {
					results[label.GetValue()] = true
				}
			}
		}
		got[family.GetName()] = results
	}

	for _, name := range []string{
		"dmwork_doc_bot_mention_ingress_total",
		"dmwork_doc_bot_mention_enqueue_total",
		"dmwork_doc_bot_mention_ingress_duration_seconds",
		"dmwork_doc_bot_mention_enqueue_duration_seconds",
	} {
		if _, ok := got[name]; !ok {
			t.Fatalf("metric family %q not registered; got=%v", name, got)
		}
		if !got[name]["accepted"] || !got[name]["error"] {
			t.Fatalf("metric %q results=%v, want accepted and sanitized error", name, got[name])
		}
	}
	for _, name := range []string{
		"dmwork_doc_bot_mention_ingress_total",
		"dmwork_doc_bot_mention_ingress_duration_seconds",
	} {
		if !got[name]["not_found"] {
			t.Fatalf("metric %q results=%v, want distinct not_found outcome", name, got[name])
		}
	}
}

func TestBotMentionKindMetricsPreserveExistingSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotMentionMetrics(registry)
	for _, kind := range []string{"", "html", "ppt", "user-controlled-value", "another-unknown"} {
		metrics.ObserveIngress("disabled", kind, time.Millisecond)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]float64{}
	var legacyTotal float64
	for _, family := range families {
		for _, metric := range family.Metric {
			switch family.GetName() {
			case "dmwork_doc_bot_mention_ingress_total":
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "result" {
					t.Fatal("existing label contract changed")
				}
				legacyTotal += metric.GetCounter().GetValue()
			case "dmwork_doc_bot_mention_ingress_by_kind_total":
				for _, label := range metric.Label {
					if label.GetName() == "doc_kind" {
						kinds[label.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	if legacyTotal != 5 || len(kinds) != 4 || kinds["legacy"] != 1 || kinds["html"] != 1 || kinds["ppt"] != 1 || kinds["unknown"] != 2 {
		t.Fatalf("total=%v kind counts=%v", legacyTotal, kinds)
	}
}
