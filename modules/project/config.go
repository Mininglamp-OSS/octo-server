package project

import (
	"fmt"
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
	// LifecycleEventProblem is why the secret was dropped, when it was. Carried
	// rather than logged at load, for the reason ProvisioningConfig.Problems is:
	// loadConfig has no logger, and a config loader that logs is a config loader
	// that cannot be tested without capturing the process-wide one.
	LifecycleEventProblem error
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
	lifecycleSecret, lifecycleProblem := resolveLifecycleEventSecret(os.Getenv)
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
		LifecycleEventSecret:   lifecycleSecret,
		LifecycleEventProblem:  lifecycleProblem,
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
// Fail-closed on UNUSABLE configuration, not just on the switch, and "unusable"
// is decided by validateLifecycleEndpoint — the same function the delivery
// client calls. That sharing is the whole point of the check.
//
// The earlier version tested only that the two strings were non-empty, so a URL
// with a query string or a secret below the key floor passed the gate and failed
// at the client. The result was an integration that enqueued on every write and
// could never build a sender: claimLifecycleEvents increments attempts before
// delivery is attempted, so every tick burned budget on rows that never reached
// the abandon check, the table grew without bound (the purge only touches
// terminal rows), and not one member revocation left the process. An outbox that
// cannot deliver is strictly worse than one that was never written.
//
// Called on the enqueue path, so it re-parses a URL per write. That is a
// url.Parse beside a database INSERT on a path that runs at project-write
// frequency; buying it back with a cached flag would put the answer somewhere it
// could disagree with the config it came from, which is the bug being fixed.
func (p *Project) lifecycleEventsEnabled() bool {
	if !p.cfg.LifecycleEventsEnabled {
		return false
	}
	return validateLifecycleEndpoint(p.cfg.LifecycleEventURL, p.cfg.LifecycleEventSecret) == nil
}

// lifecycleSecretSiblings is the set of fixed capability credentials the
// lifecycle event secret must not equal.
//
// It is main.go's fixedInternalTokenEnvs minus this env, and it exists for the
// reason that list gives: main.go LOGS a collision, while this refuses one, and
// a logged collision on a security credential is a collision that ships. The
// The fleet provisioning secret is in here too — it goes to the same peer, and
// sharing one value across the two channels means a leak from either grants
// both. (It used to say "the two provisioning secrets"; #887 collapsed Drive's
// onto OCTO_DRIVE_INTERNAL_TOKEN, so there is one provisioning secret now.)
var lifecycleSecretSiblings = []string{
	"NOTIFY_INTERNAL_TOKEN",
	"OCTO_DOCS_NOTIFY_TOKEN",
	"OCTO_DOCS_BOT_MENTION_TOKEN",
	"OCTO_DRIVE_INTERNAL_TOKEN",
	"OCTO_MEMBERSHIP_INTERNAL_TOKEN",
	ProvisionFleetSecretEnv,
	// Drive's provisioning credential is NOT a separate entry: main's #887 pointed
	// project provisioning at the existing OCTO_DRIVE_INTERNAL_TOKEN, listed four
	// lines up, rather than giving Drive its own per-target secret. Naming it twice
	// in a list whose job is finding duplicates is how such a list fails silently.
	"TS_WEBHOOK_SECRET_KEY",
	"OCTO_MAIL_GATEWAY_SECRET",
	"TS_GRPC_AUTH_TOKEN",
	// Added when main's #827 landed the Space internal API's token: this list is
	// defined as main.go's registry minus this module's own env, so an entry missing
	// here is a collision this module would accept and main.go would only log.
	"OCTO_MARKETPLACE_INTERNAL_TOKEN",
}

// resolveLifecycleEventSecret loads the secret, and returns "" plus a reason
// when it collides with a sibling credential.
//
// Dropping the secret rather than returning it with a warning is what makes the
// refusal load-bearing: lifecycleEventsEnabled() requires a non-empty secret, so
// an empty one disables BOTH the enqueue and the worker. The feed goes dark and
// says why, instead of running on a credential that grants a second capability.
//
// The refusal is symmetric with checkSecretExclusivity, which carries the
// reciprocal entry — so a collision between this secret and a provisioning one
// disables both channels rather than picking a winner. That is the intended
// outcome: with one value serving two capabilities there is no half that is
// safe to keep.
//
// Messages name ENVs, never values.
func resolveLifecycleEventSecret(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", nil
	}
	secret := strings.TrimSpace(getenv(LifecycleEventSecretEnv))
	if secret == "" {
		return "", nil
	}
	for _, sibling := range lifecycleSecretSiblings {
		if v := strings.TrimSpace(getenv(sibling)); v != "" && v == secret {
			return "", fmt.Errorf(
				"%s must differ from %s; the lifecycle event feed is disabled until it does",
				LifecycleEventSecretEnv, sibling)
		}
	}
	return secret, nil
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
