package project

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-server/internal/projectprovision"
)

// Provisioning targets. Low-cardinality enum: these two strings reach the
// `target` column, a metric label and log lines, so they are constants rather
// than operator-supplied text.
const (
	// TargetFleet is octo-fleet, whose container is a `workspace`.
	TargetFleet = "fleet"
	// TargetDrive is octo-drive, whose container is a shared `drive_space`.
	TargetDrive = "drive"
)

// Environment knobs for eager subsystem provisioning (brief D2, Shape S).
//
// Exported where main.go needs the value: the two secret envs are fed into
// cardactiondispatch.Registry.ValidateNotifyTokenExclusions, which is the ONLY
// place that also sees the dynamic per-route notify tokens and callback secrets
// loaded from OCTO_CARD_ACTION_ROUTES. This module can check its two secrets
// against each other and against the four fixed sibling internal-token envs, but
// it cannot see route-level credentials — so without that call a single leaked
// value could authorize BOTH provisioning into a subsystem AND minting a card
// action, which is exactly the "one credential / one capability" invariant the
// exclusions guard exists to hold.
const (
	// ProvisionFleetSecretEnv / ProvisionDriveSecretEnv are the per-target HMAC
	// secrets. One per target on purpose: a leaked fleet secret must not also
	// provision drive containers.
	ProvisionFleetSecretEnv = "OCTO_PROJECT_PROVISION_FLEET_SECRET"
	ProvisionDriveSecretEnv = "OCTO_PROJECT_PROVISION_DRIVE_SECRET"

	// envProvisionTargets is the enablement switch, and it is a LIST rather than
	// a boolean so the two subsystems can be turned on independently — they are
	// on different teams' schedules (brief Q1 recommends fleet first).
	//
	// Empty (the default) means no row is ever enqueued and the worker never
	// starts, i.e. project creation behaves exactly as it does on P0. That is the
	// deliberate reading of the brief's "the backlog is real work sitting
	// visible": a backlog is informative once an operator has declared they want
	// a target, and is pure noise before that. Enqueueing for an unconfigured
	// target would manufacture two guaranteed-dead rows per project on every
	// deployment on earth, and drive them all the way to `abandoned` — which is
	// the module's alert state — before anyone had asked for the feature.
	envProvisionTargets = "OCTO_PROJECT_PROVISION_TARGETS"

	envProvisionFleetURL = "OCTO_PROJECT_PROVISION_FLEET_URL"
	envProvisionDriveURL = "OCTO_PROJECT_PROVISION_DRIVE_URL"

	// envProvisionFleetNarrowed / envProvisionDriveNarrowed record whether that
	// subsystem has declared it narrows authorization by Project (fleet's R2,
	// drive's R3).
	//
	// This is the GLOBAL per-subsystem capability switch the brief requires, and
	// it is deliberately NOT the same thing as the per-project mapping row: D12
	// forbids gating any read path on that row, because `status = ready` records
	// "we called successfully once", not "the container exists now", so one stale
	// row would lock a tab forever with no user-triggerable repair.
	//
	// PR-5 discloses no container id at all, so today this switch has exactly one
	// consumer: the unnarrowed-container gauge, i.e. the size of the surface that
	// is protected only by the opaque id (brief R1). It is what a later slice will
	// gate disclosure on.
	envProvisionFleetNarrowed = "OCTO_PROJECT_PROVISION_FLEET_NARROWED"
	envProvisionDriveNarrowed = "OCTO_PROJECT_PROVISION_DRIVE_NARROWED"

	envProvisionInterval    = "OCTO_PROJECT_PROVISION_INTERVAL"
	envProvisionTimeout     = "OCTO_PROJECT_PROVISION_TIMEOUT"
	envProvisionMaxAttempts = "OCTO_PROJECT_PROVISION_MAX_ATTEMPTS"
	envProvisionBatch       = "OCTO_PROJECT_PROVISION_BATCH"

	// Sibling FIXED internal-token envs. A provisioning secret must differ from
	// every one of them so a single leaked value cannot grant two capabilities.
	// Same intra-set guard as modules/internal_resolve/config.go; the dynamic
	// route-level credentials are covered centrally in main.go (see above).
	siblingNotifyTokenEnv     = "NOTIFY_INTERNAL_TOKEN"
	siblingDocsNotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
	siblingBotMentionTokenEnv = "OCTO_DOCS_BOT_MENTION_TOKEN"
	siblingDriveInternalToken = "OCTO_DRIVE_INTERNAL_TOKEN"
)

// Provisioning defaults.
const (
	// defaultProvisionInterval is the fallback claim tick. A successful create
	// also nudges the worker immediately (see nudgeProvisioningWorker), so this
	// bounds how late a job can start after a pod restart, not the normal case.
	defaultProvisionInterval = 15 * time.Second
	// defaultProvisionTimeout bounds one ensure call.
	defaultProvisionTimeout = 10 * time.Second
	// defaultProvisionMaxAttempts, together with the capped exponential backoff,
	// gives roughly a one-hour window before a row is abandoned.
	//
	// A row is only ever enqueued for a target an operator has explicitly turned
	// on (see envProvisionTargets), so exhaustion means "a target I asked for is
	// not answering" — a real alert rather than the expected state. That is why
	// the budget is deliberately NOT stretched to days: an abandoned row has no
	// automatic re-drive, and hiding a broken target behind a week of retries
	// would delay the only signal there is.
	defaultProvisionMaxAttempts uint32 = 12
	// defaultProvisionBatch bounds one tick, so a backlog cannot occupy every DB
	// connection or open a burst of outbound calls at once.
	defaultProvisionBatch = 20
)

// provisionTarget is one resolved destination plus the two facts about it that
// are octo-server's own: whether it is enabled, and whether it has declared
// Project narrowing.
type provisionTarget struct {
	projectprovision.Target
	// Narrowed mirrors envProvision*Narrowed. False means containers on this
	// target are protected by the opacity of their id alone.
	Narrowed bool
}

// ProvisioningConfig is the resolved per-process provisioning configuration.
//
// It holds secrets. Nothing in this package logs or serializes it, and nothing
// should start: a %+v of this struct in a log line would publish both HMAC
// secrets. The provisioning worker logs target NAMES only.
type ProvisioningConfig struct {
	// Targets is the enabled, VALIDATED set. A target named in
	// OCTO_PROJECT_PROVISION_TARGETS but misconfigured is absent from here — see
	// loadProvisioningConfig for why that is a dropped target plus a loud gauge
	// rather than a refusal to boot.
	Targets []provisionTarget
	// Misconfigured holds the names of targets that were asked for and rejected,
	// so the gauge can publish them and the reason is not log-only.
	Misconfigured []string
	// Problems carries one error per rejected target, for New() to log at
	// construction. Kept on the struct rather than returned separately so
	// loadConfig keeps its single-value signature; nothing reads it after boot.
	Problems    []error
	Interval    time.Duration
	Timeout     time.Duration
	MaxAttempts uint32
	BatchSize   int
}

// Enabled reports whether any target is live. When false, createProjectOnce
// enqueues nothing and the worker does not start, so the provisioning slice is
// completely inert — which is its default state.
func (c ProvisioningConfig) Enabled() bool { return len(c.Targets) > 0 }

// TargetByName returns the resolved target, or false when it is not enabled.
func (c ProvisioningConfig) TargetByName(name string) (provisionTarget, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return provisionTarget{}, false
}

// loadProvisioningConfig resolves the provisioning configuration from the
// environment, returning the config plus one error per rejected target.
//
// A rejected target is DROPPED, not fatal, and the asymmetry with main.go's
// token-exclusion check is deliberate. That check refuses to boot because a
// credential collision is a security invariant that holds for the whole process.
// A typo in a project provisioning URL is not: taking down every IM feature for
// every tenant because one optional collaboration integration was misconfigured
// is a worse outcome than that integration being off. So the failure is loud in
// two channels that survive a restart loop — an Error log at construction and
// project_provisioning_target_misconfigured — and project creation keeps working.
//
// getenv is a parameter so the boot-config test can drive every branch without
// mutating process environment.
func loadProvisioningConfig(getenv func(string) string) (ProvisioningConfig, []error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := ProvisioningConfig{
		Interval:    envDurationFrom(getenv, envProvisionInterval, defaultProvisionInterval),
		Timeout:     envDurationFrom(getenv, envProvisionTimeout, defaultProvisionTimeout),
		MaxAttempts: uint32(envPositiveIntFrom(getenv, envProvisionMaxAttempts, int(defaultProvisionMaxAttempts))),
		BatchSize:   envPositiveIntFrom(getenv, envProvisionBatch, defaultProvisionBatch),
	}
	requested := parseTargetList(getenv(envProvisionTargets))
	if len(requested) == 0 {
		return cfg, nil
	}

	var problems []error
	secrets := map[string]string{}
	for _, name := range requested {
		urlEnv, secretEnv, narrowedEnv, ok := targetEnvNames(name)
		if !ok {
			problems = append(problems, fmt.Errorf("project provisioning: unknown target %q in %s", name, envProvisionTargets))
			cfg.Misconfigured = append(cfg.Misconfigured, name)
			continue
		}
		secret := getenv(secretEnv)
		target := provisionTarget{
			Target: projectprovision.Target{
				Name:      name,
				EnsureURL: strings.TrimSpace(getenv(urlEnv)),
				Secret:    secret,
				Timeout:   cfg.Timeout,
			},
			Narrowed: envBoolFrom(getenv, narrowedEnv, false),
		}
		if err := projectprovision.ValidateTarget(target.Target); err != nil {
			problems = append(problems, err)
			cfg.Misconfigured = append(cfg.Misconfigured, name)
			continue
		}
		if err := checkSecretExclusivity(getenv, name, secret, secrets); err != nil {
			problems = append(problems, err)
			cfg.Misconfigured = append(cfg.Misconfigured, name)
			continue
		}
		secrets[name] = secret
		cfg.Targets = append(cfg.Targets, target)
	}
	cfg.Problems = problems
	return cfg, problems
}

// checkSecretExclusivity refuses a secret that is reused across targets or shared
// with a fixed sibling internal token.
//
// Messages never carry the value, and never carry its length either.
func checkSecretExclusivity(getenv func(string) string, name, secret string, already map[string]string) error {
	for otherName, otherSecret := range already {
		if secret == otherSecret {
			return fmt.Errorf("project provisioning: %s secret must differ from the %s secret", name, otherName)
		}
	}
	for _, siblingEnv := range []string{
		siblingNotifyTokenEnv,
		siblingDocsNotifyTokenEnv,
		siblingBotMentionTokenEnv,
		siblingDriveInternalToken,
	} {
		if sibling := getenv(siblingEnv); sibling != "" && sibling == secret {
			return fmt.Errorf("project provisioning: %s secret must differ from %s", name, siblingEnv)
		}
	}
	return nil
}

// targetEnvNames maps a target name to its three envs. A switch rather than
// string concatenation so an unknown name is rejected instead of silently
// resolving to three envs nobody sets.
func targetEnvNames(name string) (urlEnv, secretEnv, narrowedEnv string, ok bool) {
	switch name {
	case TargetFleet:
		return envProvisionFleetURL, ProvisionFleetSecretEnv, envProvisionFleetNarrowed, true
	case TargetDrive:
		return envProvisionDriveURL, ProvisionDriveSecretEnv, envProvisionDriveNarrowed, true
	default:
		return "", "", "", false
	}
}

// parseTargetList splits and normalizes the target list, dropping blanks and
// duplicates while preserving order.
func parseTargetList(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// errProvisioningDisabled is returned by the worker when it is asked to run a job
// for a target that is no longer enabled. Not an outbound failure: it means
// configuration changed under a row that was already enqueued.
var errProvisioningDisabled = errors.New("project: provisioning target not enabled")

// The three env readers below mirror config.go's envBool / envPositiveInt /
// envDuration exactly, differing only in taking the lookup function so the
// boot-config test can drive them without mutating process environment. They are
// intentionally not a refactor of the originals: config.go's versions are on the
// path of every module load, and changing their signature is churn for no gain.
func envBoolFrom(getenv func(string) string, key string, fallback bool) bool {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envPositiveIntFrom(getenv func(string) string, key string, fallback int) int {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return fallback
	}
	// strconv.Atoi, not fmt.Sscanf("%d"): Sscanf stops at the first non-digit and
	// reports success, so "12abc" would silently resolve to 12 and a typo would be
	// invisible. config.go's envPositiveInt makes the same choice.
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envDurationFrom(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
