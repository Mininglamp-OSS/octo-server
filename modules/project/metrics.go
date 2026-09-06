package project

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Project metrics. promauto registers into the global default Registry, and the
// /metrics endpoint already exists and is being served (pkg/metrics/http.go,
// started from main.go), so these are scraped the moment the module loads —
// same wiring as modules/space's removal-cleanup gauges.
//
// No space_id / project_id / uid labels anywhere: those are unbounded and would
// blow up Prometheus memory.
const metricNamespace = "project"

// Admission-rejection entry points. The breakdown is the point of the metric, not
// decoration: P1 adds several more membership write paths (group admission, the
// removing intermediate state), and a single undifferentiated counter cannot tell
// you that one of them forgot to check I1. Adding a path without adding its entry
// value here is the failure this label exists to expose.
const (
	entryMemberAdd      = "member_add"
	entryRoleChange     = "role_change"
	entryCreateOwner    = "create_owner_seat"
	entryLeave          = "leave"
	entryMemberRemove   = "member_remove"
	entryProjectCreate  = "project_create"
	entryProjectUpdate  = "project_update"
	entryProjectDisband = "project_disband"
)

// Rejection reasons. Low-cardinality enum; never a free-form message.
const (
	reasonNotSpaceMember   = "not_space_member"
	reasonQuotaMembers     = "quota_members"
	reasonQuotaPerSpace    = "quota_per_space"
	reasonQuotaPerCreator  = "quota_per_creator"
	reasonQuotaDailyCreate = "quota_daily_create"
	reasonProjectDisbanded = "project_disbanded"
	reasonFlagOff          = "flag_off"
	reasonPermissionDenied = "permission_denied"
	reasonLastOwner        = "last_owner"
	reasonNameDuplicated   = "name_duplicated"
)

var (
	// writeRejected counts refused project writes, split by the entry point that refused
	// and why. Named write_rejected rather than admission_rejected: the entry set covers
	// every refused write (create/update/disband/leave/remove too, per S-3 in the round-1
	// review), and "admission" described only the member-admission slice of what it counts.
	// The entry breakdown is still what exposes a write path that skipped invariant I1.
	writeRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "write_rejected_total",
		Help: "Refused project writes, labeled by entry point and reason. " +
			"The entry breakdown is what exposes a write path that skipped invariant I1.",
	}, []string{"entry", "reason"})

	// projectTotal / memberTotal are refreshed on the sparse metrics tick.
	projectTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "active_total",
		Help:      "Number of active projects.",
	})
	memberTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "active_member_total",
		Help:      "Number of active project memberships across all projects.",
	})
	// memberCountDistribution is the per-project member-count distribution. Buckets
	// stop just past the default 500 cap so the top bucket means "at or over quota".
	memberCountDistribution = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "member_count",
		Help:      "Per-project active member count, sampled on the metrics tick.",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 200, 500, 1000},
	})

	// i1Violations is the reconcile verdict AFTER the in-flight exemption. A
	// non-zero value means a Project seat outlives its Space seat with nothing
	// scheduled to fix it.
	i1Violations = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "i1_violations",
		Help: "Project memberships with no active Space seat, excluding pairs with a pending " +
			"cleanup job and excluding banned Spaces.",
	})
	// i1AbandonedLeak is a DIFFERENT alert from i1Violations and the difference is
	// the whole point. A pending cleanup job is a normal, bounded window. An
	// abandoned one has exhausted its retry budget, nothing re-drives it, and the
	// member keeps their Project seat until a human intervenes. Folding the two
	// together trains the on-call to ignore both.
	i1AbandonedLeak = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "i1_abandoned_cleanup_leak",
		Help: "Project memberships still active behind an ABANDONED Space-removal cleanup job. " +
			"Nothing re-drives these; a non-zero value needs manual repair.",
	})
	// orphanProjects counts active projects whose Space row is gone.
	orphanProjects = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "orphan_total",
		Help:      "Active projects whose space_id no longer exists in `space`.",
	})
	// ownerlessProjects counts ACTIVE projects with zero active owners.
	//
	// A separate signal from orphan_total because the failure is separate: the Space is fine,
	// the project is reachable, and its roster may be full — but nobody can manage it. P0 has
	// no repair path (role change and disband are owner-only, a Space admin has read access
	// only), so like i1_abandoned_cleanup_leak this is a standing figure needing a human, not
	// a transient that clears.
	//
	// It exists because the state was reachable and invisible: the concurrency route is now
	// closed (see countActiveOwnersTx), and the remaining route — a sole owner removed from
	// the Space — is a filed product decision. This gauge is what lets that decision be made
	// from data rather than a guess.
	ownerlessProjects = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "ownerless_total",
		Help: "Active projects with zero active owners. Unmanageable and unrepairable in P0; " +
			"a non-zero value needs manual intervention.",
	})
	// epochAnomalies counts observed member_epoch regressions. Best-effort: the
	// authoritative guarantee is the write discipline (member_epoch + 1 only),
	// because a read-only scan running on every pod cannot establish monotonicity.
	epochAnomalies = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "member_epoch_anomalies_total",
		Help: "Observed member_epoch regressions or negative values (best-effort; monotonicity " +
			"is guaranteed by the write discipline, not by this counter).",
	})

	// reconcileDuration times each scan so a scan that starts costing real time is
	// visible before it starts competing with message traffic.
	reconcileDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "reconcile_duration_seconds",
		Help:      "Duration of one reconcile scan, labeled by scan name.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"scan"})
)

// observeRejected records one refused write.
func observeRejected(entry, reason string) {
	writeRejected.WithLabelValues(entry, reason).Inc()
}
