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

// Admission-rejection entry points. The breakdown is the point of the metric,
// not decoration: each Project membership write path (add, role change, remove,
// leave, and the removing intermediate state) must account for its failures.
const (
	entryMemberAdd               = "member_add"
	entryRoleChange              = "role_change"
	entryCreateOwner             = "create_owner_seat"
	entryLeave                   = "leave"
	entryMemberRemove            = "member_remove"
	entryProjectCreate           = "project_create"
	entryProjectUpdate           = "project_update"
	entryProjectDisband          = "project_disband"
	entryProjectSetting          = "project_setting"
	entryCollaborationRoleCreate = "collaboration_role_create"
	entryCollaborationRoleRename = "collaboration_role_rename"
	entryCollaborationRoleDelete = "collaboration_role_delete"
	entryCollaborationRoleBind   = "collaboration_role_bind"
)

// Rejection reasons. Low-cardinality enum; never a free-form message.
const (
	reasonNotSpaceMember                 = "not_space_member"
	reasonQuotaMembers                   = "quota_members"
	reasonQuotaPerSpace                  = "quota_per_space"
	reasonQuotaPerCreator                = "quota_per_creator"
	reasonQuotaDailyCreate               = "quota_daily_create"
	reasonQuotaPinned                    = "quota_pinned"
	reasonProjectDisbanded               = "project_disbanded"
	reasonFlagOff                        = "flag_off"
	reasonPermissionDenied               = "permission_denied"
	reasonLastOwner                      = "last_owner"
	reasonCollaborationRoleNameInvalid   = "collaboration_role_name_invalid"
	reasonCollaborationRoleInvalid       = "collaboration_role_invalid"
	reasonCollaborationRoleTargetInvalid = "collaboration_role_target_invalid"
	reasonCollaborationRoleDuplicated    = "collaboration_role_duplicated"
	reasonQuotaCollaborationRoles        = "quota_collaboration_roles"
	reasonQuotaMemberCollaborationRoles  = "quota_member_collaboration_roles"
	// reasonProvisioningEnqueue is the outbox-write failure inside project creation.
	// Separate from a generic store failure because this slice is what made create
	// depend on that write at all; see errProvisioningEnqueueFailed.
	reasonProvisioningEnqueue = "provisioning_enqueue"
	// reasonAgentNotEligible covers every way an AI agent may be refused a seat
	// (D3/D15). One reason label, matching the single error code: splitting it
	// here would put on a dashboard the distinctions the wire deliberately hides.
	reasonAgentNotEligible = "agent_not_eligible"
)

const (
	collaborationRoleOutcomeChanged  = "changed"
	collaborationRoleOutcomeNoop     = "noop"
	collaborationRoleOutcomeRejected = "rejected"
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

	collaborationRoleWrites = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "collaboration_role_writes_total",
		Help:      "Project collaboration-role write attempts by bounded action and outcome.",
	}, []string{"action", "outcome"})
	collaborationRoleBackfillPending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "collaboration_role_backfill_pending",
		Help:      "Active projects missing built-in collaboration roles in the latest bounded page.",
	})
	collaborationRoleBackfilled = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "collaboration_role_backfilled_total",
		Help:      "Projects whose built-in collaboration-role catalog was repaired by the bounded backfill.",
	})
	collaborationRoleIntegrityViolations = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "collaboration_role_integrity_violations",
		Help:      "Orphan collaboration-role bindings or bindings on inactive seats across the latest completed bounded cursor rotation.",
	})
	collaborationRoleMaintenanceFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "collaboration_role_maintenance_failures_total",
		Help:      "Failed collaboration-role backfill or integrity-scan operations.",
	}, []string{"operation"})

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

	// spaceProjectCountDistribution is the detector for a condition PR-5 named and
	// could not otherwise see.
	//
	// The project list endpoint sorts every project visible to the caller in the
	// Space before LIMIT applies (measured in TestTheProjectListReachesItsRowsByAnIndex:
	// 0.28ms before, 7.9ms at 2000 visible projects). The escalation condition is
	// "a Space reaches the thousands", and without this histogram it would arrive
	// as user-visible latency rather than as a signal.
	//
	// Projects per Space, not the sort input itself: the sort input is the subset
	// one caller can see, which cannot be sampled without a per-caller query, and
	// this bounds it from above. Conservative in the right direction — if no Space
	// is near the threshold then no caller's page can be. Sampled on the sparse
	// metrics tick, so it costs the request path nothing.
	//
	// Buckets run past the measured 2000 so the top one means "the escalation
	// condition in the plan guard has arrived".
	spaceProjectCountDistribution = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace,
		Name:      "space_project_count",
		Help: "Active projects per Space, sampled on the metrics tick. Bounds the row count " +
			"the project list must sort per page; see the plan guard for the escalation condition.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
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
	// reconcileScanFailures counts failed scan attempts, per scan.
	//
	// Without it a failing scan is invisible: gauges publish only on a complete rotation, so a
	// scan that errors on its first page leaves its gauge at whatever it last held (or at zero,
	// forever) and the only trace is a Warn line. "Never ran" and "ran, found nothing" then look
	// identical — which is how the collation drift would have presented, on the module whose
	// purpose is to be the safety net.
	reconcileScanFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "reconcile_scan_failures_total",
		Help: "Reconcile scan attempts that returned an error, by scan. Non-zero means the " +
			"corresponding gauge is stale or never published — alert on the rate, not the value.",
	}, []string{"scan"})

	// ownerlessProjects counts ACTIVE projects with zero active owners.
	//
	// A separate signal from orphan_total because the failure is separate: the Space is fine,
	// the project is reachable, and its roster may be full — but nobody can manage it. P0 has
	// no repair path (role change and disband are owner-only, a Space admin has read access
	// only), so like i1_abandoned_cleanup_leak this is a standing figure needing a human, not
	// a transient that clears.
	//
	// It exists because legacy or manual writes can still leave this state and
	// normal Space-removal now deliberately preserves Owner rows. The gauge lets
	// operators distinguish that historical/inconsistent state from a healthy
	// project instead of inferring it from a request failure.
	ownerlessProjects = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: metricNamespace,
		Name:      "ownerless_total",
		Help: "Active projects with zero active owners. Unmanageable and unrepairable in P0; " +
			"a non-zero value needs manual intervention.",
	})
	// epochAnomalies counts observed member_epoch regressions AND sentinel repairs.
	// Best-effort for the regression half: the authoritative guarantee is the write
	// discipline (member_epoch + 1 only), because a read-only scan running on every
	// pod cannot establish monotonicity.
	//
	// The sentinel-repair increment is the one operators are told to act on — it
	// means an instance is still writing 0 — so it belongs in the Help text. It was
	// missing, and the rollout doc named this series by its Go identifier, so the
	// alert rule an operator would have written did not exist.
	//
	// Scrapes as `project_member_epoch_anomalies_total`.
	epochAnomalies = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace,
		Name:      "member_epoch_anomalies_total",
		Help: "member_epoch anomalies: observed regressions or negative values (best-effort; " +
			"monotonicity is guaranteed by the write discipline, not by this counter), plus " +
			"each ACTIVE project found on the reserved absent-epoch sentinel and repaired — " +
			"that last case means an instance is still inserting at the old column default.",
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

func observeCollaborationRoleWrite(action, outcome string) {
	collaborationRoleWrites.WithLabelValues(action, outcome).Inc()
}

// ---------------------------------------------------------------------------
// P1 — group attribution and removal machinery
// ---------------------------------------------------------------------------

// i3Violations counts groups whose project_id points at a project that is
// disbanded, in another Space, or absent.
var i3Violations = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "i3_violations_total",
	Help:      "Groups whose project attribution is disbanded, cross-Space or missing.",
})

// removingStalls counts seats stuck mid-removal past the stall threshold.
//
// A stalled seat is a worker-health signal, not a native group-membership
// violation: authorization excludes a seat as soon as removing=1, while the
// worker retains it in storage until registered cleanup completes.
var removingStalls = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "removing_stalled_total",
	Help:      "Project seats sitting at removing=1 past the stall threshold.",
})

// removalBacklog counts pending cascade jobs.
var removalBacklog = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "removal_backlog_total",
	Help:      "Pending project member-removal cascade jobs.",
})

// removalAbandoned counts cascade jobs that ran out of attempts.
//
// The two answer different questions and the second is the one that pages: an
// abandoned job is terminal while its seat remains at removing = 1, requiring
// an operator to inspect last_error and decide how to recover. The stall gauge
// notices it only after the seat has sat there past the threshold; this counter
// moves at the moment the job gives up.
//
// A counter rather than a gauge: it is an event, and a gauge derived from a
// COUNT would go back to zero as soon as the retention purge ran.
var removalAbandoned = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: metricNamespace,
	Name:      "removal_abandoned_total",
	Help:      "Project member-removal cascade jobs abandoned after exhausting their attempts.",
})

// ---------- project lifecycle outbox (O4) ----------
//
// Labels stay low-cardinality: event_type is a closed set of five, result and
// error_class are small enums. No project_id, space_id or uid anywhere, for the
// reason stated at the top of this file.

// lifecycleEventEnqueued counts events written into the outbox, by type.
//
// Paired with lifecycleEventOutcome, the two make the outbox auditable without
// reading the table: enqueued minus delivered minus abandoned is what is still
// owed to the peer, and a divergence between that and the backlog gauge means
// rows are leaving by a path nobody intended.
var lifecycleEventEnqueued = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: metricNamespace,
	Name:      "lifecycle_event_enqueued_total",
	Help:      "Project lifecycle events written to the outbox, by event type.",
}, []string{"event_type"})

// lifecycleEventOutcome counts terminal delivery outcomes.
//
// result is delivered | abandoned. error_class is the classification from
// lifecycle_client.go for anything that is not a success, and empty on success --
// it is what tells an operator whether the peer is down (retryable, transient)
// or whether this deployment is misconfigured or in conflict (terminal), which
// need completely different responses.
var lifecycleEventOutcome = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: metricNamespace,
	Name:      "lifecycle_event_delivery_total",
	Help:      "Terminal outcomes of project lifecycle event delivery.",
}, []string{"event_type", "result", "error_class"})

// lifecycleEventAttempts counts every attempt, including the ones that will retry.
var lifecycleEventAttempts = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: metricNamespace,
	Name:      "lifecycle_event_attempt_total",
	Help:      "Project lifecycle event delivery attempts, by outcome class.",
}, []string{"event_type", "error_class"})

// lifecycleEventBacklog is how many events are still owed to the peer.
var lifecycleEventBacklog = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "lifecycle_event_backlog",
	Help:      "Project lifecycle events pending delivery.",
})

// lifecycleEventOldestAgeSeconds is the age of the oldest undelivered event, and it
// is the gauge an alert should fire on rather than the backlog count.
//
// A count cannot tell a healthy burst from a stuck queue; both read as "many
// rows". Age can. An undelivered member revocation an hour old is a removed
// member still executing on the peer, whether it is alone in the queue or has a
// thousand siblings.
var lifecycleEventOldestAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "lifecycle_event_oldest_age_seconds",
	Help:      "Age of the oldest pending project lifecycle event.",
})

// awaitingActivation is how many projects the peer still cannot see (O6).
//
// Every one of these is a project a user created and can use in this
// application, while the subsystem side has not confirmed a container — so the
// peer answers about it as if it did not exist. A steady small number is normal
// (creates in flight); a number that only grows means the confirming step has
// stopped.
var awaitingActivation = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "awaiting_activation",
	Help:      "Active projects whose subsystem container has not been confirmed, so the peer treats them as nonexistent.",
})

// awaitingActivationOldestAgeSeconds is the one to alert on, for the reason the
// lifecycle queue's age gauge exists: a count cannot separate a burst of fresh
// creates from a queue that stopped, and both read as "several rows".
var awaitingActivationOldestAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
	Namespace: metricNamespace,
	Name:      "awaiting_activation_oldest_age_seconds",
	Help:      "Age of the oldest project still awaiting subsystem confirmation.",
})
