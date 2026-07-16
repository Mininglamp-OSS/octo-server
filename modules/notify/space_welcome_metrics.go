package notify

import "sync/atomic"

// space_welcome_metrics.go — lightweight, in-process counters for the space
// welcome pipeline.
//
// Observability is intentionally minimal for this iteration (structured logs
// are the primary operator signal; Grafana wiring is deferred). These atomic
// counters are process-local and nil-safe, and are read directly by tests to
// assert enqueue-vs-dedup and send-outcome behaviour. Promoting them to
// Prometheus later is a drop-in: add a collector that samples these values.
type spaceWelcomeMetrics struct {
	enqueueEvent      atomic.Int64
	enqueueReconciler atomic.Int64
	dedupEvent        atomic.Int64
	dedupReconciler   atomic.Int64
	sendSuccess       atomic.Int64
	sendFailed        atomic.Int64
	sendUnknown       atomic.Int64
	skipNonMember     atomic.Int64
	configInvalid     atomic.Int64
	sweepClaimed      atomic.Int64
	sweepDispatching  atomic.Int64
}

func newSpaceWelcomeMetrics() *spaceWelcomeMetrics { return &spaceWelcomeMetrics{} }

// enqueue source labels.
const (
	swSourceEvent      = "event"
	swSourceReconciler = "reconciler"
)

func (m *spaceWelcomeMetrics) incEnqueue(source string) {
	if m == nil {
		return
	}
	if source == swSourceReconciler {
		m.enqueueReconciler.Add(1)
	} else {
		m.enqueueEvent.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incEnqueueDedup(source string) {
	if m == nil {
		return
	}
	if source == swSourceReconciler {
		m.dedupReconciler.Add(1)
	} else {
		m.dedupEvent.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incSendSuccess() {
	if m != nil {
		m.sendSuccess.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incSendFailed() {
	if m != nil {
		m.sendFailed.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incSendUnknown() {
	if m != nil {
		m.sendUnknown.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incSkipNonMember() {
	if m != nil {
		m.skipNonMember.Add(1)
	}
}

func (m *spaceWelcomeMetrics) incConfigInvalid() {
	if m != nil {
		m.configInvalid.Add(1)
	}
}

func (m *spaceWelcomeMetrics) addSweepClaimed(n int64) {
	if m != nil && n > 0 {
		m.sweepClaimed.Add(n)
	}
}

func (m *spaceWelcomeMetrics) addSweepDispatching(n int64) {
	if m != nil && n > 0 {
		m.sweepDispatching.Add(n)
	}
}
