package project

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment knobs. Every limit is read through this file so no handler carries
// a literal — the acceptance criterion is "each limit is read from config, not a
// literal", and a config struct built once per process is what makes that true
// for the quota checks as well as the worker cadence.
const (
	// envCreateEnabled is the fail-closed feature gate. Off (the default) makes
	// create / update / disband / member-write return 403 while list and detail
	// keep working, so existing data stays observable during a rollback.
	//
	// Env rather than system_setting: the brief's acceptance pins this PR's diff
	// to new files, and a system_setting entry needs the schema allow-list plus a
	// getter in modules/common. Moving it there later is one function; the call
	// site (createEnabled) does not change. The cost of env is that flipping it
	// needs a rolling restart in both directions.
	envCreateEnabled            = "OCTO_PROJECT_CREATE_ENABLED"
	envCollaborationRoleEnabled = "OCTO_PROJECT_COLLABORATION_ROLE_ENABLED"

	// envReconcileEnabled gates ONLY the reconcile scans that JOIN the legacy Space tables
	// (`space`, `space_member`, `space_member_removal_cleanup`) — FIVE of them, in two
	// blocks: I1 violations, abandoned cleanup leak and orphan projects
	// (reconcile.go, the first gated block), plus the two I4 all-member-group scans
	// (the second). The startup Warn names all five; comments that say "three" are
	// counting one block and are the reason this sentence now says where to look.
	//
	// It exists because those three fail with MySQL 1267 on any database where the legacy
	// tables have drifted to utf8mb4_0900_ai_ci while this module's tables are pinned to
	// utf8mb4_general_ci, which is measured to be the case in production (see the migration
	// header and the task brief). The failure is at statement RESOLUTION, so an empty table
	// does not save it: merging with no projects and no traffic would still run three failing
	// scans on every pod every tick, forever. Each affected gauge publishes only on a COMPLETE
	// rotation, so they would never publish — the monitor for this module's own safety net
	// would read as "healthy, zero violations" while having never run once.
	//
	// Default OFF, and deliberately NOT tied to envCreateEnabled even though that would also
	// have closed the merge-time hole: those two answer different questions. Turning writes off
	// to stop a problem is exactly when the invariant monitor is most wanted, and coupling them
	// would darken it at that moment.
	//
	// Scope is deliberately narrow. project_ownerless_total and the member_epoch sanity scan
	// touch only this module's own tables, as does the distribution-metrics job, so they run
	// unconditionally — gating them would lose working observability for nothing, and would
	// teach the reader that "no monitoring until the flag is on", which is not true.
	//
	// Turn it on once the collation conversion recorded in the brief has completed.
	envReconcileEnabled = "OCTO_PROJECT_RECONCILE_ENABLED"

	envMaxPerSpace                    = "OCTO_PROJECT_MAX_PER_SPACE"
	envMaxPerCreator                  = "OCTO_PROJECT_MAX_PER_CREATOR_PER_SPACE"
	envMaxMembers                     = "OCTO_PROJECT_MAX_MEMBERS"
	envMaxPinned                      = "OCTO_PROJECT_MAX_PINNED"
	envMaxDailyCreate                 = "OCTO_PROJECT_MAX_DAILY_CREATE"
	envMemberBatchMax                 = "OCTO_PROJECT_MEMBER_BATCH_MAX"
	envCollaborationRoleMaxPerProject = "OCTO_PROJECT_COLLABORATION_ROLE_MAX_PER_PROJECT"
	envCollaborationRoleMaxPerMember  = "OCTO_PROJECT_COLLABORATION_ROLE_MAX_PER_MEMBER"
	envDayBoundaryTZ                  = "OCTO_PROJECT_DAY_BOUNDARY_TZ"
	envReconcileEvery                 = "OCTO_PROJECT_RECONCILE_INTERVAL"
	envReconcileLimit                 = "OCTO_PROJECT_RECONCILE_LIMIT"
	// envAllMemberGroupAdmitGrace tunes how long a freshly written project seat is
	// exempt from I4 scan B. The brief calls the window configurable and the first
	// implementation hard-coded it; the value that matters is deployment-shaped
	// (how slow the admitter's IM call is under load), so it belongs in an env var
	// beside the other reconcile knobs rather than in a constant.
	envAllMemberGroupAdmitGrace = "OCTO_PROJECT_ALL_MEMBER_GROUP_ADMIT_GRACE"
	envMetricsEvery             = "OCTO_PROJECT_METRICS_INTERVAL"

	// envLifecycleEventsEnabled gates the project lifecycle outbox — BOTH the
	// enqueue side and the delivery worker, on the same switch.
	//
	// One switch for both, deliberately. Queueing while delivery is off produces a
	// backlog the consumer can never accept: it never saw the project.created that
	// should have preceded those events, so each one refers to something it does
	// not know about. Enabling the integration on a deployment that already has
	// projects therefore needs a deliberate backfill, and making that obvious is
	// worth more than the events a split switch would have preserved.
	//
	// Default OFF, like every other switch in this module.
	envLifecycleEventsEnabled = "OCTO_PROJECT_LIFECYCLE_EVENTS_ENABLED"
	// envLifecycleEventURL is the absolute POST endpoint events are delivered to,
	// path included. Absolute rather than a base URL with a path appended here,
	// for the reason internal/projectprovision gives: the signature covers the
	// PATH, so a path this side assembles and a path the peer serves must be the
	// same string, and the only way to be sure is to configure the whole thing.
	envLifecycleEventURL = "OCTO_PROJECT_LIFECYCLE_EVENT_URL"
	// LifecycleEventSecretEnv is the HMAC secret for outbound lifecycle events.
	//
	// A shared secret rather than a bearer token, matching the provisioning
	// channel (pkg/octosign) rather than inventing a second scheme. Two reasons,
	// and the first is the one main.go's credential registry states: a bearer
	// token we SEND is a credential the peer could send back to us, while an HMAC
	// secret proves possession without ever crossing the wire. The second is that
	// the signature covers method, path, timestamp, event id and body, so it binds
	// the request; a bearer authenticates the caller and says nothing about what
	// was sent.
	//
	// Exported so main.go can include it in the cross-capability exclusion checks
	// without a second copy of the literal drifting out of sync.
	LifecycleEventSecretEnv = "OCTO_PROJECT_LIFECYCLE_EVENT_SECRET"
	// envLifecycleEventTimeout bounds ONE delivery attempt.
	envLifecycleEventTimeout = "OCTO_PROJECT_LIFECYCLE_EVENT_TIMEOUT"
)

// Defaults. The three project/member caps come from the brief; the batch cap and
// the worker cadence are chosen here.
const (
	defaultMaxPerSpace   = 1000
	defaultMaxPerCreator = 100
	defaultMaxMembers    = 500
	// defaultMaxPinned caps how many projects one user may pin PER SPACE.
	//
	// Per Space, not globally: the pinned section is rendered inside one Space's
	// project list, so a global budget would let pinning in a Space the caller is
	// looking at be refused because of pins in a Space they cannot see — a limit
	// whose cause is invisible from where it is enforced.
	defaultMaxPinned      = 6
	defaultMaxDailyCreate = 20
	// defaultMemberBatchMax bounds one add/remove request structurally, on top of
	// any byte cap: a well-formed payload of ten thousand uids would otherwise turn
	// a single request into ten thousand membership transactions.
	defaultMemberBatchMax                 = 200
	defaultCollaborationRoleMaxPerProject = 50
	defaultCollaborationRoleMaxPerMember  = 8
	// defaultDayBoundaryTZ is the business timezone the per-day creation window is
	// computed in. Rows store UTC; only the window boundary is localized, so the
	// quota resets at local midnight rather than at 08:00 local.
	defaultDayBoundaryTZ = "Asia/Shanghai"
	// defaultReconcileInterval is deliberately much sparser than the space
	// module's 10s cleanup poll. The reconcile job detects a condition whose own
	// repair path is a human, and its scans read space_member / space; running it
	// often would compete with message traffic for the same connections for no
	// gain (modules/space/member_removal.go:281-284 makes the same call).
	defaultReconcileInterval = 5 * time.Minute
	defaultReconcileLimit    = 500
	// defaultAllMemberGroupAdmitGrace: generous against a path measured in
	// hundreds of milliseconds, short relative to how long a real gap persists
	// (nothing retries the admission, so a genuine failure stays until an admin
	// re-adds the member). See (*Project).admitGrace's own comment.
	defaultAllMemberGroupAdmitGrace = 5 * time.Minute
	// defaultMetricsInterval is sparser still: the distribution gauges aggregate
	// whole tables, and those aggregates get slowest exactly when the numbers
	// matter most (after a backlog).
	defaultMetricsInterval = 15 * time.Minute

	// defaultLifecycleEventTimeout bounds ONE delivery attempt to the peer.
	//
	// Short on purpose. A delivery that hangs holds a worker slot and its lease for
	// the whole duration, and the queue behind it is where an unsent member
	// revocation waits. Failing fast and retrying with backoff drains a temporarily
	// slow peer better than waiting on each attempt does.
	defaultLifecycleEventTimeout = 10 * time.Second
)

// Field length caps follow the Project contract. Name is measured in Unicode
// code points (the HTTP layer uses utf8.RuneCountInString), not bytes.
const (
	maxNameChars        = 30
	maxDescriptionChars = 500
	maxLogoChars        = 200
)

// Config is the resolved per-process configuration.
type Config struct {
	CreateEnabled            bool
	CollaborationRoleEnabled bool
	// ReconcileEnabled gates the five scans that JOIN legacy Space tables, in two
	// blocks — see envReconcileEnabled for the list, for why it is separate from
	// CreateEnabled, and for why its scope is narrow.
	ReconcileEnabled               bool
	MaxPerSpace                    int
	MaxPerCreator                  int
	MaxMembers                     int
	MaxPinned                      int
	MaxDailyCreate                 int
	MemberBatchMax                 int
	CollaborationRoleMaxPerProject int
	CollaborationRoleMaxPerMember  int
	DayBoundary                    *time.Location
	ReconcileInterval              time.Duration
	ReconcileLimit                 int
	// AllMemberGroupAdmitGrace exempts a project seat written within this window
	// from I4 scan B, because the admitter runs after the seat transaction commits.
	AllMemberGroupAdmitGrace time.Duration
	MetricsInterval          time.Duration

	// Project lifecycle outbox.
	LifecycleEventsEnabled bool
	LifecycleEventURL      string
	LifecycleEventSecret   string
	LifecycleEventTimeout  time.Duration
	// Provisioning is the eager subsystem-container configuration (brief D2).
	// Zero value = inert: no outbox row is enqueued and no worker starts, which is
	// the default until an operator names a target. See config_provisioning.go.
	Provisioning ProvisioningConfig
}

// loadConfig resolves the configuration from the environment once, at module
// construction. Values are not hot-reloaded: an env-sourced gate cannot be, and
// pretending otherwise would put a stale read on the request path.
func loadConfig() Config {
	loc, err := time.LoadLocation(envString(envDayBoundaryTZ, defaultDayBoundaryTZ))
	if err != nil || loc == nil {
		// An unparsable timezone must not silently become UTC on one replica and
		// Asia/Shanghai on another: fall back to the documented default, and if
		// even that fails (a container without tzdata) use UTC so the daily quota
		// still has a consistent boundary within the process.
		if loc, err = time.LoadLocation(defaultDayBoundaryTZ); err != nil || loc == nil {
			loc = time.UTC
		}
	}
	provisioning, _ := loadProvisioningConfig(os.Getenv)
	return Config{
		CreateEnabled:                  envBool(envCreateEnabled, false),
		CollaborationRoleEnabled:       envBool(envCollaborationRoleEnabled, false),
		ReconcileEnabled:               envBool(envReconcileEnabled, false),
		MaxPerSpace:                    envPositiveInt(envMaxPerSpace, defaultMaxPerSpace),
		MaxPerCreator:                  envPositiveInt(envMaxPerCreator, defaultMaxPerCreator),
		MaxMembers:                     envPositiveInt(envMaxMembers, defaultMaxMembers),
		MaxPinned:                      envPositiveInt(envMaxPinned, defaultMaxPinned),
		MaxDailyCreate:                 envPositiveInt(envMaxDailyCreate, defaultMaxDailyCreate),
		MemberBatchMax:                 envPositiveInt(envMemberBatchMax, defaultMemberBatchMax),
		CollaborationRoleMaxPerProject: envPositiveInt(envCollaborationRoleMaxPerProject, defaultCollaborationRoleMaxPerProject),
		CollaborationRoleMaxPerMember:  envPositiveInt(envCollaborationRoleMaxPerMember, defaultCollaborationRoleMaxPerMember),
		DayBoundary:                    loc,
		ReconcileInterval:              envDuration(envReconcileEvery, defaultReconcileInterval),
		ReconcileLimit:                 envPositiveInt(envReconcileLimit, defaultReconcileLimit),
		AllMemberGroupAdmitGrace: envDuration(
			envAllMemberGroupAdmitGrace, defaultAllMemberGroupAdmitGrace),
		MetricsInterval: envDuration(envMetricsEvery, defaultMetricsInterval),

		LifecycleEventsEnabled: envBool(envLifecycleEventsEnabled, false),
		LifecycleEventURL:      strings.TrimSpace(envString(envLifecycleEventURL, "")),
		LifecycleEventSecret:   envString(LifecycleEventSecretEnv, ""),
		LifecycleEventTimeout:  envDuration(envLifecycleEventTimeout, defaultLifecycleEventTimeout),
		// Rejected targets are dropped rather than fatal; the reasons ride along on
		// ProvisioningConfig.Problems for New() to log. See loadProvisioningConfig.
		Provisioning: provisioning,
	}
}

// effectiveMaxMembers resolves the per-project cap: a positive per-row override
// wins, otherwise the process default.
func (c Config) effectiveMaxMembers(perProject int) int {
	if perProject > 0 {
		return perProject
	}
	return c.MaxMembers
}

// dayWindow returns the [start, end) UTC bounds of the business day containing
// now, so the daily-create count is a half-open range scan on
// idx_octo_project_creator_created instead of DATE(created_at)=?, which is a
// function call on the column and cannot use an index.
func (c Config) dayWindow(now time.Time) (time.Time, time.Time) {
	local := now.In(c.DayBoundary)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.DayBoundary)
	return start.UTC(), start.AddDate(0, 0, 1).UTC()
}

// lifecycleEventsEnabled reports whether the outbox may enqueue and deliver.
//
// Fail-closed on INCOMPLETE configuration, not just on the switch: an enabled
// integration with no URL or no secret cannot deliver anything, so letting it
// ENQUEUE would build a backlog that only grows and whose head keeps failing on
// the same misconfiguration. Refusing at the enqueue side keeps the outbox empty
// and leaves the misconfiguration visible in the startup log instead.
func (p *Project) lifecycleEventsEnabled() bool {
	return p.cfg.LifecycleEventsEnabled &&
		p.cfg.LifecycleEventURL != "" &&
		p.cfg.LifecycleEventSecret != ""
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envBool mirrors the repo's existing switch parsing (1/true/yes/on, case
// insensitive, surrounding space tolerated) so operators do not have to remember
// a second dialect. Anything else is false — for a fail-closed gate that is the
// safe direction.
func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envPositiveInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
