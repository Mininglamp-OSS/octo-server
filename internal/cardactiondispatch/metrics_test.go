package cardactiondispatch

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsExposeBoundedDispatchAndQueueSignals(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	started := time.Now().Add(-time.Second)

	metrics.beginLease("docs")
	metrics.observeRetry("docs")
	metrics.observeError("docs", "consumer_5xx")
	metrics.observeApplicantNotifyFailure("docs")
	metrics.finish("docs", "retry", started)
	metrics.setDepths(2, 1)

	if got := testutil.ToFloat64(metrics.leased.WithLabelValues("docs")); got != 1 {
		t.Fatalf("leased gauge = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.retries.WithLabelValues("docs")); got != 1 {
		t.Fatalf("retry counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.errors.WithLabelValues("docs", "consumer_5xx")); got != 1 {
		t.Fatalf("error counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.applicantNotifyFailures.WithLabelValues("docs")); got != 1 {
		t.Fatalf("notify failure counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.dlqDepth); got != 1 {
		t.Fatalf("DLQ depth = %v, want 1", got)
	}
	metrics.endLease("docs")

	// Untrusted labels never create attacker-controlled cardinality.
	metrics.observeError("../../arbitrary", "arbitrary-error-body")
	if got := testutil.ToFloat64(metrics.errors.WithLabelValues("invalid", "unknown")); got != 1 {
		t.Fatalf("normalized error counter = %v, want 1", got)
	}
}
