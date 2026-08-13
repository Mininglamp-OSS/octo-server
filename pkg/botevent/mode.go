package botevent

// #697: which allocator this process may use, and how it decides.
//
// # The invariant, in one line
//
//	The authority decides. The mirror can only shortcut, never override.
//
// # Why this file exists (review P1-1, P1-2, P1-4)
//
// An earlier revision spread that decision across three call sites and got three
// different answers out of it:
//
//   - The hot path trusted a positive mirror outright and never read the authority,
//     so a stray `SET botEventSeq:mode incr` performed the activation the operator
//     tool exists to gate: floor validation, "no pre-fix replica remains" and the
//     `-yes` confirmation all bypassed, on every replica at once. (P1-2)
//   - The pre-activation path read the authority on *every* allocation, inside a
//     held msgSem slot, so "merging is behaviour-neutral" was false — it delegated
//     to GenSeq after a synchronous DB round trip. (P1-1)
//   - The mirror-missing path would return a GenSeq id even for a bot this process
//     had already issued counter ids for, i.e. an event deliberately placed below a
//     cursor a client demonstrably holds. (P1-4)
//
// All three are one missing thing: no single stateful entry point for "is the
// counter activated". This is it.
//
// # The cache is asymmetric, and the asymmetry is the design
//
// D2 in the task brief says the mode must not be cached per process, because one
// replica running on a stale `legacy` while another runs on `incr` is two live id
// sources — the defect itself. That reasoning stands, and what it forbids is
// *deciding the gate from a cache*: the gate still reads the uncached mirror inside
// the same Lua script as the INCR, so a flip still takes effect on the next
// allocation everywhere. What is cached here is the *authority behind* the mirror,
// under two deliberately different rules:
//
//   - A **positive** belief (activated) is terminal with respect to legacy. Once
//     this process has resolved `incr` it may refresh the epoch upward, but it can
//     never decide `legacy` again — not on a DB error, not on a rolled-back row, not
//     on a dropped table. That is what makes P1-4 unreachable rather than merely
//     unlikely: the branch that returned a low GenSeq id after the counter era began
//     is gone, not guarded.
//   - A **negative** belief (not activated) is trusted only while the mirror agrees
//     with it. A mirror claiming `incr` against a cached `legacy` is a **conflict**,
//     and a conflict forces a fresh authority read. This is what preserves D2's
//     property: propagation of the flip never waits for a TTL, because the first
//     allocation that sees the new mirror re-reads the authority. The TTL only bounds
//     the case where the operator's mirror write failed — the tool warns when it
//     does — and even then every replica converges within one interval.
//
// A *confirmed* conflict (mirror says `incr`, authority says `legacy`) is a forged
// mirror, and the answer is `legacy`, loudly.
//
// # What that does NOT mean (review P1-A)
//
// An earlier revision of this comment said it "does not split the fleet: every replica
// reads the same authority row and reaches the same conclusion". That is not what the
// code does, and the difference mattered because the claim was load-bearing. A replica
// with a positive belief returns from resolveMode without reading the authority again,
// and with an intact mirror the gate never closes, so nothing forces it to. So if the
// authority itself regresses *after* activation — a restore from before the migration, a
// manual `UPDATE … SET mode=0`, the table dropped outside sql-migrate — the fleet does
// split: replicas already running keep allocating from the counter, while a replica that
// starts afterwards reads `legacy` and would delegate to GenSeq. Two live id sources on
// one queue, which is #697.
//
// That path is now closed, with no DB read and without depending on the env guard: the
// same round trip that reads the mirror also reports whether this bot's counter key
// exists, and a counter exists only because some replica allocated from it, which can
// only have happened after the authority said `incr`. So a cold process that sees
// `legacy` alongside a live counter refuses instead of degrading — see legacyDelegate.
//
// One residual is accepted rather than claimed away: any negative belief may delegate to
// GenSeq when this bot has no counter yet. That includes an authority that is readable but has
// regressed to legacy or missing, and an unreadable authority when the mirror does not claim
// activation. In both cases a new or idle bot has no per-bot evidence that the counter era
// began. Only `OCTO_BOTEVENT_EXPECTED_MODE=incr` closes that evidence gap, and the rollout arms
// it only after activation is verified, so the window is open by design during the flip itself.
// The operator assertion closes it after the flip and is documented as a required last step.
//
// # Why the mirror carries the epoch
//
// The mirror value is `incr:{epoch}`, not a bare `incr`, and the gate compares it
// against the exact string this process validated against the authority. So a
// hand-written `SET botEventSeq:mode incr` cannot open the gate even for a single
// allocation: carrying no generation, all it can do is force an authority read. This
// is the mechanism `octo_bot_event_seq_state.epoch` was documented as having and did
// not have. A bare `incr` is still accepted as a *claim* ("something says activated,
// go confirm it"), which is what lets a mirror written by an older build heal rather
// than wedge.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/pkg/cutover"
	"go.uber.org/zap"
)

// modeDecision is what an allocation may do about the mode.
type modeDecision int

const (
	// decideLegacy delegates to GenSeq — the pre-activation state, not a fallback.
	decideLegacy modeDecision = iota

	// decideCounter allocates from the monotonic Redis counter.
	decideCounter
)

// belief is this process's resolved view of the authority.
//
// Immutable once stored: refreshing replaces the pointer rather than mutating in
// place, so a reader on the hot path never sees a half-updated belief and needs no
// lock.
type belief struct {
	activated bool

	// epoch is the authority's generation. Meaningful only when activated.
	epoch uint64

	// cutoverFloor is the global floor validated in the same authoritative read as
	// activated/epoch. It must travel with the positive belief: re-reading the state row
	// during a later per-bot seed and treating an error as zero discards the activation
	// proof and can restart a new counter below live cursors.
	cutoverFloor int64

	// deniedMirror / deniedAt record a mirror value the authority explicitly denied, so
	// the next allocations that see the same value are answered from this belief for a
	// short cooldown instead of re-reading the authority each time.
	//
	// Without it, a mirror nobody clears costs an authority read on **every** allocation,
	// serialized behind beliefMu inside a held msgSem slot: the storm review P1-C named.
	//
	// The cooldown is deliberately much shorter than negativeBeliefTTL, and it is
	// deliberately keyed on the exact value, because a cached denial is what swallowed a
	// genuine activation once already (Jerry-Xin 🔴, round 6): `Activate` bumps epoch 0 → 1
	// and the tool then writes exactly the `incr:1` that may already have been sitting
	// there forged, so the denied value and the real one are the same bytes. Nothing
	// visible from Redis distinguishes them — only a fresh authority read does — so a
	// window is unavoidable and the choice is how long. See repairCooldown, and note the
	// caller only arms this when there is no counter to contradict the authority.
	deniedMirror string
	deniedAt     time.Time

	resolvedAt time.Time
}

// modeResolution is what resolveMode answers with.
type modeResolution struct {
	decision modeDecision

	// belief is what the caller must use for the gate: its mirrorValue() is the exact
	// string the gate compares against, so a mirror that has drifted from the validated
	// generation closes the gate rather than allocating.
	belief *belief

	// unauthorizedMirror is a mirror value the authority explicitly denied.
	//
	// The caller uses it to arm the denial cooldown — and **only** that. An earlier
	// revision had the caller *delete* the key, on the reasoning that the correct mirror
	// for a legacy authority is absent. That was a fleet-wide-outage regression (review
	// round 7, P1-1): when the party that regressed is the **authority** rather than the
	// mirror, one freshly started replica would delete the very artifact every healthy
	// activated replica's gate compares against. Their gate then returns
	// gateNotActivated, they force an authority read, find legacy against an activated
	// belief, and refuse every enqueue for every bot until the state row is restored — a
	// bookkeeping regression with zero delivery impact turned into total delivery loss by
	// a rolling restart.
	//
	// Guarding the delete on `!counterExists` was the suggested minimum fix and it does
	// narrow the window, but it does not close it: the mirror is global while a counter is
	// per-bot, so an allocation for a bot that has never received an event still has no
	// counter to object with. Since the delete's only benefit was suppressing the read
	// storm, and the cooldown suppresses that too, the delete is gone entirely rather than
	// conditioned. Nothing on the allocator's legacy path writes this key now. The activated
	// recovery path can rebuild it only after validating the authority and re-seeding the
	// current bot; the operator tool is the only writer during the supervised cutover.
	unauthorizedMirror string
}

// mirrorValue is the exact ModeKey value the gate must find for this belief.
func (b *belief) mirrorValue() string { return formatMirror(b.epoch) }

const (
	// negativeBeliefTTL bounds how long a "not activated" answer is reused.
	//
	// This is NOT how long a flip takes to propagate — a mirror claiming activation
	// forces a fresh read regardless of this TTL (see the conflict rule above). It
	// bounds only the case where the authority is already flipped and the mirror is
	// not: the operator tool's mirror write failed, or the key was lost afterwards.
	// Short enough that such a window closes on its own, long enough that the
	// pre-activation steady state costs well under one authority read per second per
	// replica instead of one per allocation.
	negativeBeliefTTL = 5 * time.Second

	// repairCooldown bounds how long a mirror the authority denied is answered from the
	// cached denial instead of re-reading the authority. See belief.deniedMirror for why it
	// exists and why it is much shorter than negativeBeliefTTL.
	//
	// One second, and the tradeoff is explicit: it is the window in which a genuine
	// activation writing the same bytes as a previously denied mirror is not noticed. That
	// window cannot be zero without re-reading the authority per allocation, and it is only
	// entered at all when a mirror claiming activation was already sitting in Redis against
	// a legacy authority — which `app cutover botevent activate` now refuses to flip on top
	// of, so the precondition is checked at the one supervised step.
	repairCooldown = time.Second

	// authorityTimeout bounds every authority read.
	//
	// Explicit rather than inherited: the default MySQL DSN sets no
	// readTimeout/writeTimeout (octo-lib config), and this read happens inside a held
	// msgSem slot, where an unbounded wait stalls message fan-out for every bot in the
	// process. Failing the read is recoverable — a stale negative belief costs one
	// interval, and a positive belief cannot be downgraded by a failed read at all.
	authorityTimeout = 300 * time.Millisecond
)

// NegativeBeliefTTL is the longest interval for which an allocator may reuse a
// pre-activation belief while the mode mirror is absent.
//
// The operator tool uses this bound in its cutover failure instructions: once the
// authority has been flipped, a failed mirror write is not a successful activation.
func NegativeBeliefTTL() time.Duration { return negativeBeliefTTL }

var (
	// activeBelief is read on the hottest producer path and written only by a
	// resolution, so it is an atomic pointer rather than a mutex-guarded var. nil
	// means "never resolved".
	activeBelief atomic.Pointer[belief]

	// beliefMu single-flights authority reads: without it, a cold start under load
	// sends one SELECT per concurrent allocation for an answer they all share.
	beliefMu sync.Mutex

	// beliefNow is time.Now, indirected so a test can age a belief without sleeping.
	beliefNow = time.Now

	// mirrorUnauthorized counts mirrors claiming an activation the authority denied.
	// The allocator deliberately leaves the mirror intact: it cannot know whether the
	// mirror or the authority regressed. Without this metric the conflict is invisible,
	// and activation remains blocked until an operator diagnoses it and removes only a
	// mirror proven to be unauthorized.
	mirrorUnauthorized = newHealCounter("dmwork_bot_event_seq_mirror_unauthorized_total",
		"Mode mirrors claiming an activation the DB authority did not confirm.")

	// authorityUnreadable counts authority reads that failed. Pre-activation this is
	// benign (legacy is what a pre-migration deploy does anyway); post-activation a
	// positive belief means it changes nothing. Either way a rising count means the
	// allocator is deciding on stale information.
	authorityUnreadable = newHealCounter("dmwork_bot_event_seq_authority_unreadable_total",
		"Authority reads that failed, leaving the allocator mode unconfirmed.")

	unauthorizedWarn = &throttledWarn{every: time.Minute}
	unreadableWarn   = &throttledWarn{every: time.Minute}

	// authorityReads counts authority reads, successful or not.
	//
	// Not a Prometheus counter: its value is in the *shape* of the I/O, which is a
	// property tests assert and an incident responder reads once ("is this fleet
	// querying the state table per allocation?"), not a time series worth a metric
	// name. The pre-activation path is supposed to issue one of these per process per
	// negativeBeliefTTL — one per allocation was the regression at review P1-1, and
	// nothing in the suite could see it, because the other pre-activation test asserts
	// the id's shape rather than the I/O's.
	authorityReads atomic.Int64
)

// AuthorityReads reports how many times the authoritative state row has been read.
func AuthorityReads() int64 { return authorityReads.Load() }

// MirrorUnauthorized reports mirrors that claimed an unconfirmed activation.
func MirrorUnauthorized() int64 { return mirrorUnauthorized.load() }

// AuthorityUnreadable reports authority reads that failed.
func AuthorityUnreadable() int64 { return authorityUnreadable.load() }

// The expected-mode deployment guard, parsed once.
//
// The three-state semantics (unset means no assertion; a *malformed* value fails
// closed instead of silently disabling the guard) live in pkg/cutover, shared
// with #627's internal/msgextraseq. The previous revision here compared a
// free-form env string against ModeIncr, so `OCTO_BOTEVENT_EXPECTED_MODE=inrc`
// was indistinguishable from unset — a typo disarming the one guard that exists
// to stop a silent downgrade (review P1-4). The error wording stays in this
// file: it explains this domain's consequences and is read mid-incident.
//
// Held behind an atomic pointer rather than package vars because the test hook
// rewrites it while production code reads it. Both current call sites are
// single-goroutine so `-race` was clean, but combining the hook with the concurrency
// test would not be, and "clean today" is not a property worth relying on (review P2-3).
var expectedMode atomic.Pointer[cutover.ExpectedMode]

func init() { expectedMode.Store(parseExpectedMode(os.Getenv(ExpectedModeEnv))) }

// ExpectedModeSpellings is the authoritative set of values ExpectedModeEnv
// accepts, and the mode each one asserts. Anything else is malformed and fails
// closed.
//
// Exported so the operator command reports the guard using the same table the
// allocator enforces it with, rather than a second hand-written copy that could
// disagree with the running server about whether a value is valid.
func ExpectedModeSpellings() map[string]int {
	return map[string]int{
		ModeLegacy: StateModeLegacy,
		ModeIncr:   StateModeIncr,
	}
}

func parseExpectedMode(raw string) *cutover.ExpectedMode {
	g := cutover.ParseExpectedMode(raw, ExpectedModeSpellings())
	return &g
}

// validateExpectedMode rejects a malformed deployment guard without needing to know
// which allocator the authority selected. allocate calls this before any Redis or DB I/O:
// a typo is process-local, stable configuration, so discovering it once per allocation
// must be both immediate and cheap.
func validateExpectedMode() error {
	g := expectedMode.Load()
	if g == nil || !g.IsMalformed() {
		return nil
	}
	return fmt.Errorf("botevent: %s is set to an unrecognised value; refusing to allocate "+
		"rather than run with a guard that silently does nothing (want %q or %q)",
		ExpectedModeEnv, ModeLegacy, ModeIncr)
}

// assertExpectedMode enforces the optional deployment guard against a resolved mode.
func assertExpectedMode(activated bool) error {
	g := expectedMode.Load()
	if g == nil {
		return nil
	}
	resolved := StateModeLegacy
	if activated {
		resolved = StateModeIncr
	}
	switch g.Check(resolved) {
	case cutover.GuardMalformed:
		return validateExpectedMode()
	case cutover.GuardMismatch:
		if !activated {
			return fmt.Errorf("botevent: %s=%s but the authority says the allocator is not activated; "+
				"refusing to fall back to the legacy allocator, whose lower ids would land below "+
				"live consumer cursors", ExpectedModeEnv, ModeIncr)
		}
		return fmt.Errorf("botevent: %s=%s but the authority says the allocator is activated; "+
			"refusing to allocate from a mode this replica was not deployed for",
			ExpectedModeEnv, ModeLegacy)
	}
	return nil
}

// formatMirror renders the mirror value for an epoch.
func formatMirror(epoch uint64) string {
	return ModeIncr + ":" + strconv.FormatUint(epoch, 10)
}

// MirrorValue is the exact ModeKey value that activates the given generation.
//
// Exported for the operator tool, which must write the same spelling the allocator
// validates. A bare `incr` would be accepted as a *claim* and then force every replica
// into an authority read on every allocation until one of them removed the key.
func MirrorValue(epoch uint64) string { return formatMirror(epoch) }

// ParseMirrorEpoch reports whether a raw ModeKey value claims activation, and for which
// generation.
//
// Exported so the operator tool can recognise a mirror that claims activation the authority
// has not granted, and refuse to flip while one is present.
func ParseMirrorEpoch(v string) (uint64, bool) {
	claims, epoch, _ := parseMirror(v)
	return epoch, claims
}

// parseMirror reports whether a mirror value claims activation.
//
// A bare `incr` claims activation without naming a generation, so it can never match
// a validated mirror value and can only ever force an authority read. A malformed
// suffix is not a claim at all: there is nothing to confirm, and treating it as one
// would turn a corrupt key into an authority read per allocation.
func parseMirror(v string) (claimsIncr bool, epoch uint64, hasEpoch bool) {
	v = strings.TrimSpace(v)
	if v == ModeIncr {
		return true, 0, false
	}
	rest, ok := cutPrefix(v, ModeIncr+":")
	if !ok {
		return false, 0, false
	}
	parsed, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return false, 0, false
	}
	return true, parsed, true
}

// cutPrefix is strings.CutPrefix, spelled out to keep this file's Go version
// requirement the same as the rest of the package.
func cutPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// resolveMode decides which allocator this allocation may use, given what the
// uncached mirror says right now.
//
// mirror is the raw ModeKey value ("" when absent). The returned belief is what the
// caller must use for the gate: its mirrorValue() is the exact string the gate
// compares against, so a mirror that has drifted from the validated generation
// closes the gate rather than allocating.
func resolveMode(ctx *config.Context, mirror string) (modeResolution, error) {
	claims, mirrorEpoch, hasEpoch := parseMirror(mirror)

	if b := activeBelief.Load(); b != nil {
		// Positive is terminal with respect to legacy.
		if b.activated {
			// Positive is not terminal with respect to a newer generation. An old
			// process must refresh before it considers rebuilding the mirror, otherwise
			// it can overwrite a legitimate incr:N+1 with its cached incr:N.
			if claims && hasEpoch && mirrorEpoch > b.epoch {
				res, err := refreshAuthority(ctx, true, mirror, true)
				if err != nil {
					return modeResolution{}, err
				}
				confirmedEpoch := uint64(0)
				if res.belief != nil {
					confirmedEpoch = res.belief.epoch
				}
				if res.belief == nil || !res.belief.activated || confirmedEpoch < mirrorEpoch {
					return modeResolution{}, fmt.Errorf("botevent: mode mirror claims newer epoch %d but "+
						"the authority only confirmed epoch %d; refusing to overwrite the newer mirror",
						mirrorEpoch, confirmedEpoch)
				}
				return res, nil
			}
			return resolutionFromBelief(b, "")
		}
		if !claims && beliefNow().Sub(b.resolvedAt) < negativeBeliefTTL {
			return resolutionFromBelief(b, "")
		}
		// A mirror claiming activation normally forces a read — that is what makes a flip
		// propagate without waiting for the TTL. The one exception is a value the authority
		// already denied to this process: re-reading per allocation then would be the
		// serialized-read storm review P1-C named, with no exit. See repairCooldown.
		if claims && mirror == b.deniedMirror && beliefNow().Sub(b.deniedAt) < repairCooldown {
			return resolutionFromBelief(b, "")
		}
	}
	return refreshAuthority(ctx, claims, mirror, false)
}

// refreshAuthority reads the authority and installs the resulting belief.
//
// mirrorClaimsIncr changes only how an *inconclusive* read is handled: with a mirror
// claiming activation, "unreadable" must not become "legacy", because legacy after
// activation issues ids below live cursors. Without one, unreadable means the same
// thing a pre-migration deploy means, and legacy is the honest answer.
//
// force skips the TTL short circuits, including the positive one. It is for the
// gate-closed path, where the whole point is that something changed the mirror underneath
// a belief this process was already acting on: serving that caller from the belief it is
// trying to re-verify would spin. It does NOT skip the *staleness* check below — a forced
// caller that arrives after somebody else already did the read it wanted is served from
// that result, so one lost mode key costs one authority read rather than one per in-flight
// allocation (review P1-C).
func refreshAuthority(ctx *config.Context, mirrorClaimsIncr bool, mirror string, force bool) (modeResolution, error) {
	seen := activeBelief.Load()
	beliefMu.Lock()
	defer beliefMu.Unlock()

	prior := activeBelief.Load()
	// Double-check under the lock: a concurrent caller may already have paid for the
	// answer this one was about to ask for.
	if prior != nil && prior != seen {
		// Somebody refreshed while this caller waited for the lock. That is usually the
		// answer this caller came for — but not when it observed a mirror claiming
		// activation and the fresh belief is negative: that belief was resolved against a
		// *different* mirror observation, so serving it here would delegate to GenSeq on
		// evidence this caller has already contradicted (review P2-3). Fall through and
		// read.
		if prior.activated {
			return resolutionFromBelief(prior, "")
		}
		if !mirrorClaimsIncr {
			return resolutionFromBelief(prior, "")
		}
	}
	if prior != nil && !force {
		if prior.activated {
			return resolutionFromBelief(prior, "")
		}
		if !mirrorClaimsIncr && beliefNow().Sub(prior.resolvedAt) < negativeBeliefTTL {
			return resolutionFromBelief(prior, "")
		}
		if mirrorClaimsIncr && mirror == prior.deniedMirror &&
			beliefNow().Sub(prior.deniedAt) < repairCooldown {
			return resolutionFromBelief(prior, "")
		}
	}

	st, err := readStateDeadlined(ctx)
	switch {
	case err == nil && st.Activated():
		return install(&belief{
			activated:    true,
			epoch:        st.Epoch,
			cutoverFloor: st.CutoverFloor,
			resolvedAt:   beliefNow(),
		}, "")

	case err == nil, errors.Is(err, ErrStateMissing):
		// The authority says legacy, or says nothing because the migration has not
		// run — which is what legacy looks like before the migration.
		if prior != nil && prior.activated {
			// Unreachable through resolveMode, which returns early on a positive
			// belief; reachable from the gate-closed path, which forces a read.
			// Asserted rather than assumed: the consequence of getting it wrong is a
			// permanently invisible event, and this is the exact shape a migration
			// rollback produces (review P1-4).
			return modeResolution{}, fmt.Errorf("botevent: the authority now says the allocator "+
				"is not activated (epoch %d, %v), but this process has already issued counter ids; "+
				"refusing to downgrade to the legacy allocator, whose ids would land below live "+
				"consumer cursors", prior.epoch, err)
		}
		unauthorized := ""
		if mirrorClaimsIncr {
			// A mirror claiming an activation the authority denies. Legacy is what the
			// authority says, so that is the answer — but somebody wrote that key, and
			// nothing else about this is visible from outside. Note this does NOT by
			// itself keep the fleet on one id source; see the P1-A section in the file
			// header for what does and what remains residual.
			mirrorUnauthorized.inc()
			unauthorizedWarn.warn("botevent: mode mirror claims activation but the authority does not; "+
				"leaving the conflicting mirror intact and allocating from the legacy allocator "+
				"only when this bot has no counter evidence; operator diagnosis is required before activation",
				zap.String("mirrorKey", ModeKey), zap.String("mirrorValue", mirror), zap.Error(err))
			unauthorized = mirror
		}
		return install(&belief{resolvedAt: beliefNow()}, unauthorized)

	default:
		// Inconclusive.
		authorityUnreadable.inc()
		if prior != nil && prior.activated {
			return modeResolution{}, fmt.Errorf("botevent: the authority is unreadable (%w) and this "+
				"process has already issued counter ids; refusing to downgrade to the legacy allocator", err)
		}
		if mirrorClaimsIncr {
			return modeResolution{}, fmt.Errorf("botevent: the mode mirror claims activation but the "+
				"authority is unreadable (%w); refusing to allocate — the counter cannot be trusted "+
				"unconfirmed, and legacy would issue ids below live cursors if the claim is true", err)
		}
		// Which of the two "legacy" reasons this was — the authority said so, or it could
		// not be read — is distinguished by *which warning fires*, here versus the branch
		// above. An earlier revision kept a `confirmed` flag on the belief for that and
		// nothing ever read it (review round 7), so the flag is gone and these two call
		// sites are the record.
		unreadableWarn.warn("botevent: allocator authority unreadable; treating as not activated",
			zap.Error(err))
		// Cache the inconclusive answer too. During a DB outage the alternative is one
		// failed read per allocation on the fan-out path, which is the cost this cache
		// exists to remove.
		return install(&belief{resolvedAt: beliefNow()}, "")
	}
}

// install stores a belief before enforcing the deployment guard and returning the
// matching resolution.
//
// The ordering is load-bearing: a guard mismatch is stable process configuration, not a
// reason to throw away a successfully parsed authority result. Keeping the belief means
// later refusals reuse it instead of serializing every message on another MySQL read. Cached
// resolutions still re-run assertExpectedMode, so retaining a negative belief cannot make an
// `EXPECTED_MODE=incr` replica fall through to GenSeq.
//
// unauthorizedMirror is passed through to the caller rather than acted on here: repairing
// it is a Redis write and this file deliberately does no Redis I/O — the allocator holds
// the client.
func install(b *belief, unauthorizedMirror string) (modeResolution, error) {
	activeBelief.Store(b)
	return resolutionFromBelief(b, unauthorizedMirror)
}

// resolutionFromBelief applies the deployment assertion every time a cached or freshly
// installed belief is used. The belief decides the candidate mode; the assertion decides
// whether this replica is permitted to act on it.
func resolutionFromBelief(b *belief, unauthorizedMirror string) (modeResolution, error) {
	if err := assertExpectedMode(b.activated); err != nil {
		return modeResolution{}, err
	}
	res := modeResolution{belief: b, unauthorizedMirror: unauthorizedMirror}
	if b.activated {
		res.decision = decideCounter
		return res, nil
	}
	res.decision = decideLegacy
	return res, nil
}

// noteMirrorUnauthorized records a mirror value the authority denied, so the next
// allocations that see the same value are answered from this belief for a short cooldown
// rather than re-reading the authority each time. See belief.deniedMirror.
//
// Copy-on-write like every other belief update: the pointer is swapped, never mutated, so
// a reader on the hot path cannot see a half-updated belief.
func noteMirrorUnauthorized(denied string) {
	beliefMu.Lock()
	defer beliefMu.Unlock()
	cur := activeBelief.Load()
	if cur == nil || cur.activated {
		// Nothing to cool down: an activated belief does not consult the mirror claim at
		// all, and with no belief the next allocation reads the authority anyway.
		return
	}
	updated := *cur
	updated.deniedMirror = denied
	updated.deniedAt = beliefNow()
	activeBelief.Store(&updated)
}

// readStateDeadlined reads the authority under authorityTimeout.
func readStateDeadlined(ctx *config.Context) (State, error) {
	authorityReads.Add(1)
	dl, cancel := context.WithTimeout(context.Background(), authorityTimeout)
	defer cancel()
	return ReadStateContext(dl, ctx)
}

// ResetModeBeliefForTest clears the cached authority belief.
func ResetModeBeliefForTest() {
	activeBelief.Store(nil)
	mirrorUnauthorized.resetForTest()
	authorityUnreadable.resetForTest()
}

// AgeModeBeliefForTest ages the cached belief past negativeBeliefTTL and past
// repairCooldown without sleeping, so a test can prove the caches expire rather than
// persist. Both timestamps move together: a test that needs one of them stale almost always
// means "make this belief old".
func AgeModeBeliefForTest() {
	if b := activeBelief.Load(); b != nil {
		aged := *b
		aged.resolvedAt = b.resolvedAt.Add(-2 * negativeBeliefTTL)
		if !b.deniedAt.IsZero() {
			aged.deniedAt = b.deniedAt.Add(-2 * negativeBeliefTTL)
		}
		activeBelief.Store(&aged)
	}
}

// SetExpectedModeForTest overrides the env-derived expectation and returns a restore
// function. Takes the raw env spelling so a test can exercise a malformed value.
func SetExpectedModeForTest(raw string) func() {
	prev := expectedMode.Load()
	expectedMode.Store(parseExpectedMode(raw))
	return func() { expectedMode.Store(prev) }
}
