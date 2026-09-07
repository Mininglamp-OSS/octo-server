package project

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Provisioning metrics. Same registration story as metrics.go: promauto into the
// default registry, which /metrics already serves.
//
// Every label value is a closed enum — `target` is TargetFleet / TargetDrive,
// `outcome` and `category` come from a fixed set in the worker and in
// internal/projectprovision. No project_id, no space_id, no container_id, and
// container_id in particular must never appear anywhere in this file: it is a
// capability until R2/R3 land, and a metric label is as public as a log line.
var (
	// provisioningRows is the row census, by target and status. One gauge with a
	// status label rather than four gauges: the interesting quantity is almost
	// always a ratio between two of them (pending vs ready, abandoned vs total),
	// and four separate gauges make that a join in the dashboard.
	provisioningRows = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "provisioning_rows",
		Help: "Provisioning outbox rows by target and status " +
			"(pending / ready / abandoned / disband_pending), sampled on the metrics tick.",
	}, []string{"target", "status"})

	// provisioningUnnarrowedContainers is the size of the exposed surface, and it
	// is the reason this slice can ship before another team's work.
	//
	// A container that exists before anyone visits it is a container whose access
	// control has not been narrowed yet (brief P-3). Until a target declares
	// Project narrowing — fleet's R2, drive's R3 — the only thing between such a
	// container and every member of its Space is the opacity of its id (R1). This
	// gauge answers "how many containers are in that state", without asking the
	// other repository anything.
	provisioningUnnarrowedContainers = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "provisioning_unnarrowed_containers",
		Help: "Containers provisioned into a target that has NOT yet declared it narrows " +
			"authorization by Project. Protected by the opacity of the container id alone.",
	}, []string{"target"})

	// provisioningTargetMisconfigured is 1 for a target that was requested via
	// OCTO_PROJECT_PROVISION_TARGETS and rejected at config load.
	//
	// A gauge as well as a startup Error log, because a log line is lost in a
	// restart loop and the whole point of dropping (rather than refusing to boot)
	// on a bad URL is that the process keeps running — which would otherwise make
	// the misconfiguration completely silent after the first minute.
	provisioningTargetMisconfigured = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "provisioning_target_misconfigured",
		Help: "1 when a requested provisioning target was rejected at config load " +
			"(bad URL, short secret, credential collision) and is therefore not provisioning.",
	}, []string{"target"})

	// provisioningAttempts counts finished attempts by target and outcome.
	//
	// **Exactly one increment per attempt**, and the invariant is worth stating because
	// the first version broke it in both directions: two failure paths incremented zero
	// times, and the attempt that exhausted the budget incremented twice under two
	// different labels. So `sum by(outcome)` IS the attempt count, and a rate on any one
	// outcome is a real fraction of traffic. See releaseOrAbandon, which is the single
	// increment site for every failure.
	//
	// `outcome` is the low-cardinality classification from
	// internal/projectprovision.Category, plus "ready" for success and this module's own
	// "panic" / "target_disabled" / "unclassified". A permanent contract break
	// (target_no_ensure_endpoint, container_id_mismatch) is therefore distinguishable
	// from a transient one (target_5xx, transport_failed) on the FIRST attempt rather
	// than only once the row abandons ~23 minutes later.
	//
	// There is deliberately no "abandoned" outcome: "how many rows gave up" is a
	// different question, already answered by provisioning_rows{status="abandoned"}, and
	// as a counter label it competed with the real reason for the same increment.
	provisioningAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "provisioning_attempts_total",
		Help: "Provisioning ensure attempts, by target and outcome. Exactly one increment per " +
			"attempt, so sum by(outcome) is the attempt count.",
	}, []string{"target", "outcome"})

	// provisioningDuration times one ensure call.
	provisioningDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "provisioning_duration_seconds",
		Help:      "Duration of one subsystem ensure call, by target.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"target"})
)

// provisioningStatusLabel maps a status to its metric label. A switch rather than
// a number so a dashboard query does not have to encode the enum.
func provisioningStatusLabel(status uint8) string {
	switch status {
	case provisionStatusPending:
		return "pending"
	case provisionStatusReady:
		return "ready"
	case provisionStatusAbandoned:
		return "abandoned"
	case provisionStatusDisbandPending:
		return "disband_pending"
	default:
		return "unknown"
	}
}

// observeProvisioningAttempt records one finished attempt.
func observeProvisioningAttempt(target, outcome string) {
	provisioningAttempts.WithLabelValues(target, outcome).Inc()
}
