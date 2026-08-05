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
// mirror. The answer is `legacy`, loudly, and it does not split the fleet: every
// replica reads the same authority row and reaches the same conclusion, so
// consistency comes from the authority rather than from the mirror. That sentence is
// what the previous revision was missing, and it is why trusting the authority over
// the mirror is safe where trusting a cache would not be.
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

	// confirmed distinguishes "the authority said legacy" from "the authority could
	// not be read and legacy is what we do about it". Both behave identically and
	// both expire; the flag exists so the log says which one happened.
	confirmed bool

	resolvedAt time.Time
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

	// authorityTimeout bounds every authority read.
	//
	// Explicit rather than inherited: the default MySQL DSN sets no
	// readTimeout/writeTimeout (octo-lib config), and this read happens inside a held
	// msgSem slot, where an unbounded wait stalls message fan-out for every bot in the
	// process. Failing the read is recoverable — a stale negative belief costs one
	// interval, and a positive belief cannot be downgraded by a failed read at all.
	authorityTimeout = 300 * time.Millisecond
)

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
	// It self-heals to legacy, so without this metric a forged or stale mirror is
	// invisible — no error, no failed enqueue, nothing to alert on.
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
// Same shape as #627's internal/msgextraseq: unset means no assertion, and a
// *malformed* value fails closed instead of silently disabling the guard. The
// previous revision compared a free-form env string against ModeIncr, so
// `OCTO_BOTEVENT_EXPECTED_MODE=inrc` was indistinguishable from unset — a typo
// disarming the one guard that exists to stop a silent downgrade (review P1-4).
var (
	expectedModeSet       bool
	expectedModeIncr      bool
	expectedModeMalformed bool
)

func init() { loadExpectedMode(os.Getenv(ExpectedModeEnv)) }

func loadExpectedMode(raw string) {
	expectedModeSet, expectedModeIncr, expectedModeMalformed = false, false, false
	switch strings.TrimSpace(raw) {
	case "":
	case ModeLegacy:
		expectedModeSet = true
	case ModeIncr:
		expectedModeSet, expectedModeIncr = true, true
	default:
		expectedModeSet, expectedModeMalformed = true, true
	}
}

// assertExpectedMode enforces the optional deployment guard against a resolved mode.
func assertExpectedMode(activated bool) error {
	if !expectedModeSet {
		return nil
	}
	if expectedModeMalformed {
		return fmt.Errorf("botevent: %s is set to an unrecognised value; refusing to allocate "+
			"rather than run with a guard that silently does nothing (want %q or %q)",
			ExpectedModeEnv, ModeLegacy, ModeIncr)
	}
	if expectedModeIncr && !activated {
		return fmt.Errorf("botevent: %s=%s but the authority says the allocator is not activated; "+
			"refusing to fall back to the legacy allocator, whose lower ids would land below "+
			"live consumer cursors", ExpectedModeEnv, ModeIncr)
	}
	if !expectedModeIncr && activated {
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
// into an authority read on every allocation until one of them rewrote the key.
func MirrorValue(epoch uint64) string { return formatMirror(epoch) }

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
func resolveMode(ctx *config.Context, mirror string) (modeDecision, *belief, error) {
	claims, _, _ := parseMirror(mirror)

	if b := activeBelief.Load(); b != nil {
		// Positive is terminal with respect to legacy.
		if b.activated {
			return decideCounter, b, nil
		}
		// Negative is trusted only while the mirror agrees.
		if !claims && beliefNow().Sub(b.resolvedAt) < negativeBeliefTTL {
			return decideLegacy, b, nil
		}
	}
	return refreshAuthority(ctx, claims, false)
}

// refreshAuthority reads the authority and installs the resulting belief.
//
// mirrorClaimsIncr changes only how an *inconclusive* read is handled: with a mirror
// claiming activation, "unreadable" must not become "legacy", because legacy after
// activation issues ids below live cursors. Without one, unreadable means the same
// thing a pre-migration deploy means, and legacy is the honest answer.
//
// force skips the cached-answer short circuits, including the positive one. It is for
// the gate-closed path, where the whole point is that something changed the mirror
// underneath a belief this process was already acting on: serving that caller from the
// belief it is trying to re-verify would spin.
func refreshAuthority(ctx *config.Context, mirrorClaimsIncr, force bool) (modeDecision, *belief, error) {
	beliefMu.Lock()
	defer beliefMu.Unlock()

	prior := activeBelief.Load()
	// Double-check under the lock: a concurrent caller may already have paid for the
	// answer this one was about to ask for.
	if prior != nil && !force {
		if prior.activated {
			return decideCounter, prior, nil
		}
		if !mirrorClaimsIncr && beliefNow().Sub(prior.resolvedAt) < negativeBeliefTTL {
			return decideLegacy, prior, nil
		}
	}

	st, err := readStateDeadlined(ctx)
	switch {
	case err == nil && st.Activated():
		if err := assertExpectedMode(true); err != nil {
			return decideLegacy, nil, err
		}
		return install(&belief{activated: true, epoch: st.Epoch, confirmed: true, resolvedAt: beliefNow()})

	case err == nil, errors.Is(err, ErrStateMissing):
		// The authority says legacy, or says nothing because the migration has not
		// run — which is what legacy looks like before the migration.
		if prior != nil && prior.activated {
			// Unreachable through resolveMode, which returns early on a positive
			// belief; reachable from the gate-closed path, which forces a read.
			// Asserted rather than assumed: the consequence of getting it wrong is a
			// permanently invisible event, and this is the exact shape a migration
			// rollback produces (review P1-4).
			return decideLegacy, nil, fmt.Errorf("botevent: the authority now says the allocator "+
				"is not activated (epoch %d, %v), but this process has already issued counter ids; "+
				"refusing to downgrade to the legacy allocator, whose ids would land below live "+
				"consumer cursors", prior.epoch, err)
		}
		if mirrorClaimsIncr {
			// A mirror claiming an activation the authority denies. Legacy is correct
			// and is what every replica concludes from the same row, so this does not
			// split the fleet — but somebody wrote that key, and nothing else about
			// this is visible from outside.
			mirrorUnauthorized.inc()
			unauthorizedWarn.warn("botevent: mode mirror claims activation but the authority does not; "+
				"ignoring the mirror and allocating from the legacy allocator",
				zap.String("mirrorKey", ModeKey), zap.Error(err))
		}
		if err := assertExpectedMode(false); err != nil {
			return decideLegacy, nil, err
		}
		return install(&belief{confirmed: true, resolvedAt: beliefNow()})

	default:
		// Inconclusive.
		authorityUnreadable.inc()
		if prior != nil && prior.activated {
			return decideLegacy, nil, fmt.Errorf("botevent: the authority is unreadable (%w) and this "+
				"process has already issued counter ids; refusing to downgrade to the legacy allocator", err)
		}
		if mirrorClaimsIncr {
			return decideLegacy, nil, fmt.Errorf("botevent: the mode mirror claims activation but the "+
				"authority is unreadable (%w); refusing to allocate — the counter cannot be trusted "+
				"unconfirmed, and legacy would issue ids below live cursors if the claim is true", err)
		}
		if guardErr := assertExpectedMode(false); guardErr != nil {
			return decideLegacy, nil, guardErr
		}
		unreadableWarn.warn("botevent: allocator authority unreadable; treating as not activated",
			zap.Error(err))
		// Cache the inconclusive answer too. During a DB outage the alternative is one
		// failed read per allocation on the fan-out path, which is the cost this cache
		// exists to remove.
		return install(&belief{resolvedAt: beliefNow()})
	}
}

// install stores a belief and returns the matching decision.
func install(b *belief) (modeDecision, *belief, error) {
	activeBelief.Store(b)
	if b.activated {
		return decideCounter, b, nil
	}
	return decideLegacy, b, nil
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

// AgeModeBeliefForTest ages the cached belief past negativeBeliefTTL without
// sleeping, so a test can prove the negative cache expires rather than persists.
func AgeModeBeliefForTest() {
	if b := activeBelief.Load(); b != nil {
		aged := *b
		aged.resolvedAt = b.resolvedAt.Add(-2 * negativeBeliefTTL)
		activeBelief.Store(&aged)
	}
}

// SetExpectedModeForTest overrides the env-derived expectation and returns a restore
// function. Takes the raw env spelling so a test can exercise a malformed value.
func SetExpectedModeForTest(raw string) func() {
	prevSet, prevIncr, prevMalformed := expectedModeSet, expectedModeIncr, expectedModeMalformed
	loadExpectedMode(raw)
	return func() {
		expectedModeSet, expectedModeIncr, expectedModeMalformed = prevSet, prevIncr, prevMalformed
	}
}
