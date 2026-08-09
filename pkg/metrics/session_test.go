package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestSessionMetricsRegisterAndObserveLowCardinalitySignals(t *testing.T) {
	prev := defaultSessionMetrics.Load()
	t.Cleanup(func() { defaultSessionMetrics.Store(prev) })

	reg := prometheus.NewRegistry()
	NewSessionMetrics(reg)
	ObserveSessionOperation("issue", time.Now().Add(-time.Millisecond), nil)
	ObserveSessionOperation("issue", time.Now().Add(-time.Millisecond), errors.New("redis unavailable"))
	ObserveSessionValidationReject("v3_expired")
	ObservePersistentSession()
	SetSessionEffectiveTTL(48 * time.Hour)

	if got := counterWithLabels(t, reg, "dmwork_session_operations_total", map[string]string{"operation": "issue", "outcome": "ok"}); got != 1 {
		t.Fatalf("issue ok = %v, want 1", got)
	}
	if got := counterWithLabels(t, reg, "dmwork_session_operations_total", map[string]string{"operation": "issue", "outcome": "error"}); got != 1 {
		t.Fatalf("issue error = %v, want 1", got)
	}
	if got := counterWithLabels(t, reg, "dmwork_session_validation_rejected_total", map[string]string{"reason": "v3_expired"}); got != 1 {
		t.Fatalf("v3_expired = %v, want 1", got)
	}
	if got := counterWithLabels(t, reg, "dmwork_session_persistent_detected_total", nil); got != 1 {
		t.Fatalf("persistent detected = %v, want 1", got)
	}
	if got := gaugeWithoutLabels(t, reg, "dmwork_session_effective_ttl_seconds"); got != (48 * time.Hour).Seconds() {
		t.Fatalf("effective ttl = %v, want %v", got, (48 * time.Hour).Seconds())
	}
}

func TestSessionMetricsNoDefaultIsNoop(t *testing.T) {
	prev := defaultSessionMetrics.Load()
	t.Cleanup(func() { defaultSessionMetrics.Store(prev) })
	defaultSessionMetrics.Store(nil)

	ObserveSessionOperation("read", time.Now(), nil)
	ObserveSessionValidationReject("missing")
	ObservePersistentSession()
	SetSessionEffectiveTTL(time.Hour)
}

func counterWithLabels(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			matched := true
			for labelName, labelValue := range want {
				found := false
				for _, label := range metric.GetLabel() {
					if label.GetName() == labelName && label.GetValue() == labelValue {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if matched {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gaugeWithoutLabels(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return 0
}
