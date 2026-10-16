package drivepolicy

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the internal drive upload-policy endpoint (design §5.2).
//
// promauto registers into the global default Registry, which /metrics already
// serves (pkg/metrics/http.go, started from main.go), so these are scraped the
// moment the module loads. No caller-IP / uid / token labels anywhere: those are
// unbounded or sensitive.

// served_total result label values. Low-cardinality enum, never a free-form
// message, and never the token / IP.
const (
	resultOk           = "ok"
	resultUnauthorized = "unauthorized"
	resultRateLimited  = "rate_limited"
	// resultUnavailable is a 503: the settings snapshot has never loaded, so
	// the effective policy is unknown and the endpoint refuses to publish a
	// default. Distinct from "ok" because a sustained rate here is the
	// drive-side fail-closed precursor, not healthy traffic.
	resultUnavailable = "unavailable"
)

// servedTotal counts every request outcome on /v1/internal/drive/upload-policy.
//
// The three-way split is the point: "unauthorized spiking" (token rotation /
// misconfiguration) and "no successful calls for 5 min" (drive-side fail-closed
// precursor) are the documented alerts, and neither is answerable from a single
// opaque success/failure counter.
var servedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "octo",
	Name:      "drive_upload_policy_served_total",
	Help: "Internal drive upload-policy endpoint request outcomes by result: " +
		"ok (2xx), unauthorized (X-Internal-Token missing / wrong / unset), " +
		"rate_limited (per-IP strict bucket rejected with 429), " +
		"unavailable (settings snapshot never loaded; refused with 503).",
}, []string{"result"})

// internalAuthFailTotal counts X-Internal-Token failures specifically, so the
// token-probing signal is separable from the overall unauthorized count even
// though they move together today.
var internalAuthFailTotal = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "octo",
	Name:      "drive_upload_policy_internal_auth_fail_total",
	Help: "Internal drive upload-policy requests rejected because the " +
		"X-Internal-Token header was missing, wrong, or the token is unset.",
})

// policyVersionInfo publishes the currently-served policy_version, so an
// operator can answer "has this instance converged on the latest policy" from
// monitoring rather than by correlating log lines.
//
// It is deliberately NOT a GaugeVec keyed by policy_version: that shape keeps
// every version the process has ever served, all stuck at 1 for the whole
// process lifetime, so an A→B policy change exports both series and the "what is
// live now" question becomes unanswerable (and the series accumulate). This
// collector holds exactly one fingerprint — the most recently served one — and
// replacing it retires the previous series, so a gather always shows a single
// sample. Set is safe for concurrent callers (and concurrent with Gather).
var policyVersionInfo = newPolicyVersionCollector()

// policyVersionCollector is a concurrent-safe, single-series collector for
// octo_drive_upload_policy_version_info. It emits nothing until the first
// successful serve, then exactly one sample whose policy_version label is the
// current fingerprint (value always 1). Last write wins.
type policyVersionCollector struct {
	mu      sync.RWMutex
	version string
	served  bool
	desc    *prometheus.Desc
}

func newPolicyVersionCollector() *policyVersionCollector {
	return &policyVersionCollector{
		desc: prometheus.NewDesc(
			"octo_drive_upload_policy_version_info",
			"Currently-served drive upload policy fingerprint (policy_version "+
				"content hash), value always 1. Exactly one sample is exported — "+
				"the most recently served version; serving a new policy replaces "+
				"(retires) the previous one instead of accumulating a series per "+
				"version.",
			[]string{"policy_version"},
			nil,
		),
	}
}

// Set records the currently-served fingerprint, retiring any previously
// published version.
func (c *policyVersionCollector) Set(version string) {
	c.mu.Lock()
	c.version = version
	c.served = true
	c.mu.Unlock()
}

// Describe reports the single (label-variable) descriptor.
func (c *policyVersionCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect emits the current fingerprint, or nothing before the first serve.
func (c *policyVersionCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.served {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1, c.version)
}

func init() {
	prometheus.MustRegister(policyVersionInfo)
}
