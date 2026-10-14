package drivepolicy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics for the internal drive upload-policy endpoint (design §5.2).
//
// promauto registers into the global default Registry, which /metrics already
// serves (pkg/metrics/http.go, started from main.go), so these are scraped the
// moment the module loads. No caller-IP / uid / token labels anywhere: those are
// unbounded or sensitive. The only label with unbounded cardinality is
// policy_version, which is bounded in practice by the number of distinct policy
// contents an operator sets — the same property octo_*_info gauges rely on.

// served_total result label values. Low-cardinality enum, never a free-form
// message, and never the token / IP.
const (
	resultOk           = "ok"
	resultUnauthorized = "unauthorized"
	resultRateLimited  = "rate_limited"
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
		"rate_limited (per-IP strict bucket rejected with 429).",
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
// monitoring rather than by correlating log lines. Set on every successful
// serve; the value is a content hash (see common.driveUploadPolicyVersion).
var policyVersionInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "octo",
	Name:      "drive_upload_policy_version_info",
	Help: "Currently-served drive upload policy fingerprint " +
		"(policy_version content hash), value always 1. Presence of a series " +
		"identifies a policy_version this instance has served.",
}, []string{"policy_version"})
