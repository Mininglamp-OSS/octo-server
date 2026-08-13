package cutover

// The expected-mode deployment guard, extracted from #627's
// internal/msgextraseq (store.go) and #697's pkg/botevent (mode.go), which
// implemented the same three-state semantics independently:
//
//   - unset makes no assertion — the default, what pre-migration deploys and
//     CleanAllTables-based test setups rely on;
//   - a recognized value asserts that the resolved mode matches, failing
//     closed on mismatch — the durable safety net that stops a lost or reset
//     state row from silently re-enabling the legacy allocator after a
//     cutover;
//   - a MALFORMED value fails closed instead of silently disabling the guard.
//     `EXPECTED_MODE=inrc` must not be indistinguishable from unset — a typo
//     disarming the one guard that exists to stop a silent downgrade was a
//     #697 review finding (P1-4).
//
// Only the decision tree lives here. Each domain keeps its own error wording:
// those messages explain domain consequences ("legacy ids would land below
// live consumer cursors") and are read by operators mid-incident, so they are
// load-bearing documentation, not boilerplate.
//
// Naming convention for the environment variable: OCTO_<DOMAIN>_EXPECTED_MODE
// (see docs/cutover-framework.md). Existing deployments keep their historical
// names; the convention binds new domains.
//
// Ordering invariant (both existing runbooks state it independently): the
// guard is armed only AFTER the flip is verified, never shipped in the same
// rolling wave — a replica expecting the active mode while the row is still
// inactive fails every write closed, a self-inflicted outage.

import "strings"

// ExpectedMode is a parsed expected-mode guard. The zero value makes no
// assertion. Values are immutable after parsing; copy freely.
type ExpectedMode struct {
	set       bool
	malformed bool
	mode      int
}

// GuardVerdict is what Check decided about a resolved mode.
type GuardVerdict int

const (
	// GuardOK: no assertion, or the resolved mode matches the expected one.
	GuardOK GuardVerdict = iota
	// GuardMalformed: the guard value was set but not recognized. Fail
	// closed — a guard that silently does nothing is worse than none.
	GuardMalformed
	// GuardMismatch: the resolved mode differs from the expectation. Fail
	// closed.
	GuardMismatch
)

// ParseExpectedMode parses a raw guard value against the domain's recognized
// spellings (e.g. {"legacy": ModeInactive, "transactional": ModeActive}).
// Whitespace is trimmed so a trailing newline from a ConfigMap does not turn
// a valid assertion into a malformed one. Empty means no assertion; any other
// unrecognized value is malformed and fails closed at Check time.
func ParseExpectedMode(raw string, values map[string]int) ExpectedMode {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ExpectedMode{}
	}
	if mode, ok := values[raw]; ok {
		return ExpectedMode{set: true, mode: mode}
	}
	return ExpectedMode{set: true, malformed: true}
}

// IsSet reports whether any value (valid or malformed) was provided.
func (e ExpectedMode) IsSet() bool { return e.set }

// IsMalformed reports whether the provided value was unrecognized.
func (e ExpectedMode) IsMalformed() bool { return e.set && e.malformed }

// Expected returns the asserted mode. ok is false when the guard is unset or
// malformed — callers must not treat the zero mode as an assertion then.
func (e ExpectedMode) Expected() (mode int, ok bool) {
	return e.mode, e.set && !e.malformed
}

// Check applies the guard's decision tree to a resolved mode. It never
// consults I/O; callers re-run it as often as they like (both existing
// domains re-check cached resolutions on every use).
func (e ExpectedMode) Check(resolved int) GuardVerdict {
	switch {
	case !e.set:
		return GuardOK
	case e.malformed:
		return GuardMalformed
	case resolved != e.mode:
		return GuardMismatch
	default:
		return GuardOK
	}
}
