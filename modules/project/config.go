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
	envCreateEnabled = "OCTO_PROJECT_CREATE_ENABLED"

	envMaxPerSpace    = "OCTO_PROJECT_MAX_PER_SPACE"
	envMaxPerCreator  = "OCTO_PROJECT_MAX_PER_CREATOR_PER_SPACE"
	envMaxMembers     = "OCTO_PROJECT_MAX_MEMBERS"
	envMaxDailyCreate = "OCTO_PROJECT_MAX_DAILY_CREATE"
	envMemberBatchMax = "OCTO_PROJECT_MEMBER_BATCH_MAX"
	envDayBoundaryTZ  = "OCTO_PROJECT_DAY_BOUNDARY_TZ"
	envReconcileEvery = "OCTO_PROJECT_RECONCILE_INTERVAL"
	envReconcileLimit = "OCTO_PROJECT_RECONCILE_LIMIT"
	envMetricsEvery   = "OCTO_PROJECT_METRICS_INTERVAL"
)

// Defaults. The three project/member caps come from the brief; the batch cap and
// the worker cadence are chosen here.
const (
	defaultMaxPerSpace    = 1000
	defaultMaxPerCreator  = 100
	defaultMaxMembers     = 500
	defaultMaxDailyCreate = 20
	// defaultMemberBatchMax bounds one add/remove request structurally, on top of
	// any byte cap: a well-formed payload of ten thousand uids would otherwise turn
	// a single request into ten thousand membership transactions.
	defaultMemberBatchMax = 200
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
	// defaultMetricsInterval is sparser still: the distribution gauges aggregate
	// whole tables, and those aggregates get slowest exactly when the numbers
	// matter most (after a backlog).
	defaultMetricsInterval = 15 * time.Minute
)

// Field length caps.
const (
	maxNameChars        = 64
	maxDescriptionChars = 500
	maxLogoChars        = 200
)

// Config is the resolved per-process configuration.
type Config struct {
	CreateEnabled     bool
	MaxPerSpace       int
	MaxPerCreator     int
	MaxMembers        int
	MaxDailyCreate    int
	MemberBatchMax    int
	DayBoundary       *time.Location
	ReconcileInterval time.Duration
	ReconcileLimit    int
	MetricsInterval   time.Duration
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
	return Config{
		CreateEnabled:     envBool(envCreateEnabled, false),
		MaxPerSpace:       envPositiveInt(envMaxPerSpace, defaultMaxPerSpace),
		MaxPerCreator:     envPositiveInt(envMaxPerCreator, defaultMaxPerCreator),
		MaxMembers:        envPositiveInt(envMaxMembers, defaultMaxMembers),
		MaxDailyCreate:    envPositiveInt(envMaxDailyCreate, defaultMaxDailyCreate),
		MemberBatchMax:    envPositiveInt(envMemberBatchMax, defaultMemberBatchMax),
		DayBoundary:       loc,
		ReconcileInterval: envDuration(envReconcileEvery, defaultReconcileInterval),
		ReconcileLimit:    envPositiveInt(envReconcileLimit, defaultReconcileLimit),
		MetricsInterval:   envDuration(envMetricsEvery, defaultMetricsInterval),
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
