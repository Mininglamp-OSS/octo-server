package bot_mention

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestBotMentionMetricsRegisterLowCardinalityResults(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newBotMentionMetrics(registry)

	metrics.ObserveIngress("accepted", 25*time.Millisecond)
	metrics.ObserveIngress("unexpected-result", 10*time.Millisecond)
	metrics.ObserveEnqueue("accepted")
	metrics.ObserveEnqueue("unexpected-result")

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
	} {
		if _, ok := got[name]; !ok {
			t.Fatalf("metric family %q not registered; got=%v", name, got)
		}
		if !got[name]["accepted"] || !got[name]["error"] {
			t.Fatalf("metric %q results=%v, want accepted and sanitized error", name, got[name])
		}
	}
}
