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

	// envProvision*ReclaimConsumerLive states, PER TARGET, that a consumer is actually
	// polling the D9 status endpoint, and it GATES THE PURGE OF THAT TARGET'S ROWS.
	//
	// The retention window is justified as a reclaim window — the subsystem learns about a
	// disband by polling POST /v1/internal/projects/status — and that endpoint does not
	// exist in this repository yet (it is PR-2, which stacks on this slice). Without a gate
	// the clock starts the day a target is enabled while nothing can poll, and once a
	// disband_pending row is purged the answer to "was this project disbanded" is
	// permanently `unknown` — which D9 makes indistinguishable from "outside your grant",
	// so the consumer can never reclaim that container. Deleting the row is the one
	// irreversible operation in this slice, so it is the one thing that must not run on an
	// assumption.
	//
	// PER TARGET, and not one process-global switch, because a consumer is a per-subsystem
	// deliverable: fleet and drive land on different teams' schedules, which is why every
	// other subsystem fact here (enablement, URL, secret, narrowing) is already per target.
	// A global switch means the FIRST subsystem to ship a consumer authorizes deletion of
	// the OTHER one's reclaim records — an irreversible loss, for a target whose consumer
	// nobody claimed was live, on the rollout order the runbook actually prescribes.
	//
	// Read for every KNOWN target rather than every ENABLED one, deliberately: a target
	// that was enabled, produced rows and was then disabled still has reclaimable
	// containers on the other side, and its live consumer must still be able to finish
	// sweeping them. Enablement governs what we CREATE; this governs what we may FORGET.
	//
	// Default OFF: retained rows cost storage, an un-reclaimable container is a leak.
	envProvisionFleetReclaimConsumerLive = "OCTO_PROJECT_PROVISION_FLEET_RECLAIM_CONSUMER_LIVE"
	envProvisionDriveReclaimConsumerLive = "OCTO_PROJECT_PROVISION_DRIVE_RECLAIM_CONSUMER_LIVE"

	// envProvisionRetiredReclaimConsumerLive is the process-global predecessor of the two
	// switches above. It is READ ONLY TO REFUSE IT.
	//
	// Silently ignoring it would fail in the safe direction (the purge stays off, rows are
	// retained) but silently: an operator who had followed an earlier revision of the
	// runbook would believe reclaim accounting was being pruned when it was not. Cheaper to
	// name it once at boot than to explain a growing table later.
	envProvisionRetiredReclaimConsumerLive = "OCTO_PROJECT_PROVISION_RECLAIM_CONSUMER_LIVE"

	// envProvisionRequeueProjectID names ONE project whose `abandoned` provisioning
	// rows are moved back to `pending` at boot, i.e. the manual re-drive for a project
	// that would otherwise be permanently invisible to the peer.
	//
	// An env rather than an endpoint or a CLI command because the deployment channel
	// available here is the configmap. Two consequences the operator has to know, both
	// inherent to that channel rather than to this implementation:
	//
	//  1. It takes effect on RESTART, because env is read once at boot. Re-driving a
	//     second project means editing the configmap and restarting again.
	//  2. The value PERSISTS in the configmap after it has been applied. Every later
	//     restart — a deploy, a node drain, an autoscale event — reads it again.
	//
	// (2) is what makes a naive reading of this env unsafe, so the requeue is NOT
	// unconditional on the value being present: requeueAbandonedProvisioningJobs only
	// matches rows currently in `abandoned`, so a second run over an already-requeued
	// project matches nothing and is a no-op. The row's own state is the idempotency
	// record — deliberately, because the alternative is a "we already handled this
	// value" ledger, which is a second source of truth about a decision the row
	// already encodes. A row that reached `abandoned` AGAIN after a requeue will be
	// re-driven by the next restart, which is the correct reading of an operator who
	// has left the instruction in place.
	//
	// Multi-replica: every pod runs this at boot and they race on the same rows. The
	// UPDATE re-checks `status = abandoned`, so exactly one pod moves each row and the
	// others match nothing — the log line reports 0 for them.
	envProvisionRequeueProjectID = "OCTO_PROJECT_PROVISION_REQUEUE_PROJECT_ID"

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
	// defaultProvisionMaxAttempts, together with the capped exponential backoff, gives a
	// ~23.5 minute window before a row is abandoned.
	//
	// The arithmetic is written out because the first version of this comment claimed
	// "roughly one hour" and that was simply wrong — the kind of slip that survives review
	// precisely because nobody adds up the series. provisioningRetryDelay is
	// min(2^attempt, 300s) and release() is called with attempts 1..11 (the 12th
	// abandons), so the schedule is 2+4+8+16+32+64+128+256+300+300+300 = 1410s. Re-do this
	// sum if you change either the cap or the attempt count.
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

// maxProvisionAttemptsCeiling bounds OCTO_PROJECT_PROVISION_MAX_ATTEMPTS.
//
// A ceiling exists because the value is cast to uint32 and an UNBOUNDED int cast is a
// silent kill switch: envPositiveIntFrom only rejects n <= 0, so
// OCTO_PROJECT_PROVISION_MAX_ATTEMPTS=4294967296 parses as a positive int and becomes
// uint32(0). Every claim predicate is `attempts < 0`, so the entire slice turns into a
// no-op from the first tick with no misconfiguration signal anywhere. Reported and
// refused now rather than clamped silently — a value an operator typed and we then
// ignored is worse than one we rejected out loud.
//
// 50 rather than something larger: past ~11 attempts every backoff step is already the
// 5-minute cap, so a bigger budget buys linear delay and nothing else, while the whole
// argument for a SHORT budget is that exhaustion is a real alert (see
// defaultProvisionMaxAttempts).
const maxProvisionAttemptsCeiling = 50

// maxProvisionTimeoutFraction is how much of the lease one ensure call may consume.
//
// This is the enforcement of a relationship that used to be only a comment. There is
// deliberately NO lease heartbeat (see provisioningLease), so nothing can extend a lease
// while a call is in flight — which means a Timeout at or above the lease lets another
// replica legitimately re-claim and run the SAME row concurrently, and lets the sweep
// write `abandoned` underneath a still-running executor. A quarter keeps the documented
// ~10x headroom for the 10s default while leaving room for an operator to raise it.
const maxProvisionTimeoutFraction = 4

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
	// ReclaimTargets is the set of target names whose reclaim consumer the operator has
	// declared live, and it is the ONLY set whose disband_pending rows the purge may
	// delete. See envProvision*ReclaimConsumerLive — with no consumer polling, purging a
	// disband_pending row makes its container permanently un-reclaimable.
	//
	// Deliberately NOT a subset of Targets: enablement governs what we create, this governs
	// what we may forget, and a target disabled after use still has containers to reclaim.
	ReclaimTargets []string
	// RequeueProjectID is the project whose abandoned rows are re-driven at boot, or
	// empty. See envProvisionRequeueProjectID for why this is boot-time and why a
	// leftover value is safe.
	RequeueProjectID string
}

// Enabled reports whether any target is live. When false, createProjectOnce
// enqueues nothing and the worker does not start.
//
// NOT completely inert, and the exception is deliberate rather than an
// oversight: markProvisioningDisbandPendingTx runs UNCONDITIONALLY on the
// disband path. Gating that on Enabled() would mean a target that was enabled,
// produced rows, and was later disabled stops marking its containers
// reclaimable — a real leak, with no local record that anything was left behind.
//
// This comment is what a future reader consults immediately before "tidying up"
// by adding that gate, so it says so here rather than only in the runbook.
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
	// Knob validation runs FIRST and unconditionally, before the target list is even
	// read. Two of these values are load-bearing for the retry state machine rather than
	// mere tuning, and an out-of-range value has to be refused with a reason whether or
	// not any target happens to be enabled today — otherwise the misconfiguration is
	// discovered the day someone turns a target on.
	var problems []error
	maxAttempts, attemptsErr := parseMaxAttempts(getenv)
	if attemptsErr != nil {
		problems = append(problems, attemptsErr)
	}
	timeout, timeoutErr := parseProvisionTimeout(getenv)
	if timeoutErr != nil {
		problems = append(problems, timeoutErr)
	}
	cfg := ProvisioningConfig{
		Interval:    envDurationFrom(getenv, envProvisionInterval, defaultProvisionInterval),
		Timeout:     timeout,
		MaxAttempts: maxAttempts,
		BatchSize:   envPositiveIntFrom(getenv, envProvisionBatch, defaultProvisionBatch),
	}
	// Resolved BEFORE the empty-target-list early return, and over every known target
	// rather than the enabled ones, because the purge must still be able to finish the
	// reclaim accounting for a target that has since been switched off while another is
	// still enabled. Enablement decides what gets created; this decides what may be deleted.
	// (With NOTHING enabled the purge tick is not scheduled at all — startProvisioningWorker
	// records why that boundary is there. This value still has to resolve, so that the
	// retired-env refusal below is reported on a rolled-back deployment too.)
	reclaimTargets, reclaimProblems := resolveReclaimTargets(getenv)
	cfg.ReclaimTargets = reclaimTargets
	problems = append(problems, reclaimProblems...)

	// Resolved before the early return too: an operator who set the requeue env while the
	// target list happens to be empty gets told it will do nothing, rather than restarting
	// and finding no trace of it either way.
	cfg.RequeueProjectID = strings.TrimSpace(getenv(envProvisionRequeueProjectID))

	requested := parseTargetList(getenv(envProvisionTargets))
	if len(requested) == 0 {
		// Saying so out loud is the whole point of resolving the value above. The requeue
		// runs from startProvisioningWorker, which returns early when no target is
		// enabled, so without this the rescue an operator reaches for during an incident
		// produces no output at any level — the exact "no trace either way" outcome the
		// comment above promises to prevent. A rollback deployment (targets cleared, a
		// leftover rescue value still in the configmap) is where this lands.
		//
		// Same reasoning as the retired reclaim env below: an operator who followed the
		// runbook has to learn that the instruction did nothing, and a startup problem is
		// the channel that survives a restart loop.
		if cfg.RequeueProjectID != "" {
			problems = append(problems, fmt.Errorf(
				"project provisioning: %s is set but %s enables no target, so no row will be requeued; "+
					"enable the target that owns the stuck row, or clear the requeue env",
				envProvisionRequeueProjectID, envProvisionTargets))
		}
		cfg.Problems = problems
		return cfg, problems
	}

	secrets := map[string]string{}
	for _, name := range requested {
		envs, ok := targetEnvNames(name)
		if !ok {
			problems = append(problems, fmt.Errorf("project provisioning: unknown target %q in %s", name, envProvisionTargets))
			cfg.Misconfigured = append(cfg.Misconfigured, name)
			continue
		}
		secret := getenv(envs.secret)
		target := provisionTarget{
			Target: projectprovision.Target{
				Name:      name,
				EnsureURL: getenv(envs.url),
				Secret:    secret,
				Timeout:   cfg.Timeout,
			},
			Narrowed: envBoolFrom(getenv, envs.narrowed, false),
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

// parseMaxAttempts resolves the retry budget, refusing a value outside
// [1, maxProvisionAttemptsCeiling] instead of casting it into a silent no-op.
//
// Returns the DEFAULT plus an error on a bad value. Falling back is right — a broken
// knob must not disable a security-adjacent control — but falling back SILENTLY is what
// made the uint32 wrap invisible, so the error is what the caller reports.
func parseMaxAttempts(getenv func(string) string) (uint32, error) {
	raw := strings.TrimSpace(getenv(envProvisionMaxAttempts))
	if raw == "" {
		return defaultProvisionMaxAttempts, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxProvisionAttemptsCeiling {
		return defaultProvisionMaxAttempts, fmt.Errorf(
			"project provisioning: %s must be an integer in [1, %d]; using the default %d",
			envProvisionMaxAttempts, maxProvisionAttemptsCeiling, defaultProvisionMaxAttempts)
	}
	return uint32(n), nil
}

// parseProvisionTimeout resolves the per-call timeout, refusing anything that is not
// comfortably below the lease.
//
// The relationship is a correctness constraint, not a preference: with no heartbeat, a
// call that outlives its lease gets a second executor on the same row and can be marked
// `abandoned` while it is still running.
func parseProvisionTimeout(getenv func(string) string) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(envProvisionTimeout))
	if raw == "" {
		return defaultProvisionTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	ceiling := provisioningLease / maxProvisionTimeoutFraction
	if err != nil || d <= 0 || d > ceiling {
		return defaultProvisionTimeout, fmt.Errorf(
			"project provisioning: %s must be a positive duration at most %s (a quarter of the %s lease, "+
				"which cannot be extended because there is no heartbeat); using the default %s",
			envProvisionTimeout, ceiling, provisioningLease, defaultProvisionTimeout)
	}
	return d, nil
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

// provisionTargetEnvs is the env set that belongs to one target. A struct rather than
// four return values: the group only grows, and positional returns of the same type are
// exactly how a url ends up read out of a secret env.
type provisionTargetEnvs struct {
	url                 string
	secret              string
	narrowed            string
	reclaimConsumerLive string
}

// provisionTargetSpec is everything this build knows about one subsystem: its
// name, its four envs, and its container-id prefix.
//
// One table, five former dispatch sites. Before this, adding a third subsystem
// meant editing allProvisionTargetNames, targetEnvNames's switch,
// containerIDPrefix's switch, refreshProvisioningMetrics's literal slice and
// provisioningTargetLabel's switch — five places, and the last two were pure
// omission risk: a target missing from them provisions correctly and is simply
// invisible on the dashboards that the slice's own security argument rests on.
// Now the table is the single place, and provisionTargetRegistry below is the
// only source any of them reads.
//
// Still a HARDCODED table rather than operator-supplied config, and that is the
// point: an unknown name in OCTO_PROJECT_PROVISION_TARGETS is rejected at boot
// (loadProvisioningConfig) precisely because the known set is compiled in. Making
// the set configurable would need its own validator for prefix shape, secret
// floor, requiredness and visibility participation — replacing one rejection that
// already works with four that would have to be written.
type provisionTargetSpec struct {
	// name is the enum value that reaches the `target` column, metric labels and
	// log lines.
	name string
	// envs are the four per-target environment variables.
	envs provisionTargetEnvs
	// containerPrefix is the human-readable container-id prefix. Constant per
	// target and carries no project information — see containerIDPrefix.
	containerPrefix string
}

// provisionTargetRegistry is every target this build knows about, enabled or not.
//
// Order is fixed and load-bearing: resolveReclaimTargets iterates it to build an
// IN () predicate, and a stable order keeps that predicate identical across
// processes.
//
// To add a subsystem: append one entry here and declare its four envs above.
// Nothing else in this package should need to learn the name.
var provisionTargetRegistry = []provisionTargetSpec{
	{
		name: TargetFleet,
		envs: provisionTargetEnvs{
			url:                 envProvisionFleetURL,
			secret:              ProvisionFleetSecretEnv,
			narrowed:            envProvisionFleetNarrowed,
			reclaimConsumerLive: envProvisionFleetReclaimConsumerLive,
		},
		containerPrefix: "octows-",
	},
	{
		name: TargetDrive,
		envs: provisionTargetEnvs{
			url:                 envProvisionDriveURL,
			secret:              ProvisionDriveSecretEnv,
			narrowed:            envProvisionDriveNarrowed,
			reclaimConsumerLive: envProvisionDriveReclaimConsumerLive,
		},
		containerPrefix: "octods-",
	},
}

// allProvisionTargetNames is every target this build knows about, enabled or not.
//
// The purge and the misconfiguration census both need to reason about a target that is
// absent from OCTO_PROJECT_PROVISION_TARGETS, so the known set cannot be derived from the
// enabled set.
func allProvisionTargetNames() []string {
	names := make([]string, 0, len(provisionTargetRegistry))
	for _, spec := range provisionTargetRegistry {
		names = append(names, spec.name)
	}
	return names
}

// lookupProvisionTarget returns the registry entry for a name.
//
// A table lookup rather than string concatenation so an unknown name is rejected
// instead of silently resolving to a set of envs nobody sets.
func lookupProvisionTarget(name string) (provisionTargetSpec, bool) {
	for _, spec := range provisionTargetRegistry {
		if spec.name == name {
			return spec, true
		}
	}
	return provisionTargetSpec{}, false
}

// targetEnvNames maps a target name to its envs.
func targetEnvNames(name string) (provisionTargetEnvs, bool) {
	spec, ok := lookupProvisionTarget(name)
	if !ok {
		return provisionTargetEnvs{}, false
	}
	return spec.envs, true
}

// resolveReclaimTargets reads the per-target reclaim switches and refuses the retired
// process-global one.
//
// Iterates allProvisionTargetNames, NOT the enabled list: see
// envProvision*ReclaimConsumerLive. Order is the fixed order of that list, so the
// resulting IN () predicate is stable across processes.
func resolveReclaimTargets(getenv func(string) string) (targets []string, problems []error) {
	for _, name := range allProvisionTargetNames() {
		envs, ok := targetEnvNames(name)
		if !ok {
			continue
		}
		if envBoolFrom(getenv, envs.reclaimConsumerLive, false) {
			targets = append(targets, name)
		}
	}
	if strings.TrimSpace(getenv(envProvisionRetiredReclaimConsumerLive)) != "" {
		problems = append(problems, fmt.Errorf(
			"project provisioning: %s is retired and IGNORED; it was replaced by the per-target %s / %s "+
				"because one subsystem declaring a live consumer must not authorize deleting the other's "+
				"reclaim records. The retention purge stays OFF for any target without its own switch",
			envProvisionRetiredReclaimConsumerLive,
			envProvisionFleetReclaimConsumerLive, envProvisionDriveReclaimConsumerLive))
	}
	return targets, problems
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
