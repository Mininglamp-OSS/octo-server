// Package internaltoken owns the registry of *fixed* internal-token
// environment variables and the single resolve function every capability that
// owns one goes through.
//
// # The invariant
//
// One credential grants exactly one capability. Each internal service-to-service
// endpoint in octo-server is gated by its own X-Internal-Token value so that a
// single leaked token cannot be replayed against a second, unrelated capability,
// and so that one consumer can be rotated or revoked without disturbing the rest.
//
// # Why a registry instead of per-module checks
//
// Before this package every module hand-rolled its own "must differ from
// sibling X" comparison, and each author only compared against the siblings
// that existed when they wrote it: notify compared against nothing, docs-notify
// against one, bot-mention against two, drive against three. That ladder
// happens to cover all six of today's pairs, but it covers them by convention —
// the Nth capability is protected only if its author remembers the full list,
// and the N-1 modules already in the tree never learn the new env exists at all.
// Coverage degrades silently as capabilities are added.
//
// The registry below makes the same ladder a property of the data instead of a
// property of what each author remembered. Adding a Spec is the whole change.
//
// # Registration order is a precedence order
//
// Resolve compares the env being resolved against every env registered BEFORE
// it, and the junior side is the one that yields. (One env opts out — see
// Spec.Mutual — but precedence is the default and the rest of this section
// describes it.) Two consequences:
//
//   - Coverage is complete by construction. Every unordered pair {i, j} is
//     compared exactly once — by whichever of the two was registered later — so
//     a newly appended Spec is guarded against all existing ones with no edit
//     anywhere else, and no pair can be missed by an author forgetting a name.
//
//   - Which capability survives a collision is decided, not arbitrary. The
//     incumbent keeps serving; the newcomer that duplicated an existing secret
//     is the one that goes dark. That is the pre-existing behaviour of the
//     hand-rolled checks, and it matters operationally: disabling BOTH sides
//     would take a working production ingress offline on the next deploy, for a
//     misconfiguration whose damage (one secret opening two doors) is already
//     contained the moment either side is disabled.
//
// So append new Specs to the END of the registry. Reordering it silently
// changes which live capability a misconfigured deployment loses.
//
// A genuinely new capability may instead want its pairs symmetric — "fail both
// closed rather than pick an arbitrary winner" — which is safe precisely
// because no deployment can yet depend on the other side staying up. That is
// what Spec.Mutual expresses, and OCTO_MARKETPLACE_INTERNAL_TOKEN (#827) is
// the entry that made the case: it shipped the mirror-image branch in all four
// existing modules by hand, while deliberately leaving every pre-existing pair
// on precedence. Registering it here replaced five hand-written copies of that
// decision with one field.
//
// # What a collision does
//
// A collision disables the capability being resolved — Resolve returns an empty
// token plus a logger-safe reason, and the caller's auth middleware then fails
// closed because it has no token to compare against. Boot is NOT failed. That is
// deliberate and pinned by TestCardActionDispatchScopesBotMentionTokenCollisionFailures
// in main_carddispatch_test.go: a collision between two *fixed* internal-token
// envs degrades module-locally, while a collision with a *dynamic* route
// credential from OCTO_CARD_ACTION_ROUTES fails startup. Promoting fixed-vs-fixed
// collisions to boot failures is a separate rollout decision.
//
// # Error strings never contain a token value
//
// Every message this package produces is assembled from env names and byte
// counts only. Reasons are logged and, in some callers, surfaced at boot;
// TestErrorsNeverContainTokenValue pins that no value can reach them.
package internaltoken

import (
	"errors"
	"fmt"
)

// Header is the wire header carrying an internal-service credential. One
// convention across every internal API in octo-server, owned here alongside the
// env names so a fourth capability cannot introduce a fourth spelling.
const Header = "X-Internal-Token"

// Fixed internal-token env names. Exported so callers pass a constant rather
// than a literal that can drift out of sync with the registry.
const (
	// NotifyInternalTokenEnv gates the legacy text-notification ingress in
	// modules/notify.
	NotifyInternalTokenEnv = "NOTIFY_INTERNAL_TOKEN"

	// DocsNotifyTokenEnv gates the docs-card notification ingress in
	// modules/notify. Distinct from the legacy token so the docs consumer
	// cannot mint arbitrary legacy notifications.
	DocsNotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"

	// BotMentionTokenEnv gates the doc-comment bot-mention ingress in
	// modules/bot_mention.
	BotMentionTokenEnv = "OCTO_DOCS_BOT_MENTION_TOKEN"

	// DriveInternalTokenEnv gates the drive-facing resolve endpoints in
	// modules/internal_resolve. The docs-auto-mount polling loop in octo-drive
	// presents this token on every call.
	DriveInternalTokenEnv = "OCTO_DRIVE_INTERNAL_TOKEN"

	// MarketplaceInternalTokenEnv gates the Space role lookup in modules/space
	// (GET /v1/internal/spaces/:space_id/members/:uid/role). It authorizes
	// reading any uid's role in any Space, which is why its collisions are
	// Mutual rather than precedence-ordered — see the Spec.Mutual doc.
	MarketplaceInternalTokenEnv = "OCTO_MARKETPLACE_INTERNAL_TOKEN"
)

// DefaultMinBytes is the repository-wide length floor for internal-route
// credentials. internal/cardactiondispatch applies the same constant to the
// dynamic per-route callback secrets and notify tokens, so operators have one
// bar to remember and raising it here raises it everywhere.
//
// "Everywhere" is not one blast radius, though, and raising this constant is a
// rollout decision on the same footing as the MinBytes waivers below. A FIXED
// env below the floor is disabled module-locally and boot succeeds. A DYNAMIC
// route credential below the floor fails validateRouteSpec, which propagates
// out of installCardActionDispatch and PANICS main. So a bump from 32 to, say,
// 48 would quietly disable a fixed ingress AND hard-fail boot for any
// deployment whose route callback secret or route notify token is 32-47 bytes.
const DefaultMinBytes = 32

// Spec describes one fixed internal-token env. Registry position is meaningful:
// see "Registration order is a precedence order" in the package doc.
type Spec struct {
	// Env is the environment variable name.
	Env string
	// Capability names what the token unlocks, in prose. It is interpolated
	// into reasons as "<Env> ...; <Capability> disabled", so keep it a noun
	// phrase.
	Capability string
	// Mutual makes this env's collisions symmetric: a shared value disables
	// BOTH sides, not just the junior one.
	//
	// The default (false) is the precedence rule described above — the
	// incumbent keeps serving, the newcomer yields — which is what the four
	// original envs had already built by hand and what a refactor must not
	// silently change for a live deployment.
	//
	// Mutual is for an env whose owner deliberately chose "fail both closed
	// rather than pick an arbitrary winner" AND is new enough that no
	// deployment can already be relying on the other side staying up. That is
	// exactly the trade-off #827 made for MarketplaceInternalTokenEnv: it added
	// the mirror-image branch to all four existing modules precisely because it
	// was introducing the pair, while leaving every pre-existing pair alone for
	// the same reason the default is precedence.
	//
	// Set it only with that justification. Flipping an existing entry from
	// false to true takes a currently-serving ingress down on the next deploy
	// of any installation carrying the collision.
	Mutual bool

	// MinBytes is the length floor enforced on the value. Zero means no floor.
	//
	// New capabilities MUST use DefaultMinBytes. The three legacy envs below
	// carry an explicit zero because they shipped without a floor: raising
	// their bar would disable a live capability on the next deploy of any
	// installation whose value is shorter, which is a rollout decision on the
	// same footing as promoting collisions to boot failures — not something a
	// refactor gets to do silently. Flipping one of these to DefaultMinBytes is
	// a one-line change once that rollout happens.
	MinBytes int
}

// registry is the source of truth, in precedence order: an entry yields to
// every entry above it. APPEND new capabilities; do not reorder.
var registry = []Spec{
	{Env: NotifyInternalTokenEnv, Capability: "legacy internal notify API", MinBytes: 0},
	{Env: DocsNotifyTokenEnv, Capability: "docs notification capability", MinBytes: 0},
	{Env: BotMentionTokenEnv, Capability: "bot mention capability", MinBytes: 0},
	{Env: DriveInternalTokenEnv, Capability: "drive internal API", MinBytes: DefaultMinBytes},
	{Env: MarketplaceInternalTokenEnv, Capability: "Space role lookup", MinBytes: DefaultMinBytes, Mutual: true},
}

// Reason classifies why Resolve refused a token, so callers can pick a log
// level (an unset env is a normal "capability not configured"; a collision is
// an operator error worth an ERROR line) without string matching.
type Reason string

const (
	// ReasonUnavailable: no getenv was supplied.
	ReasonUnavailable Reason = "lookup_unavailable"
	// ReasonUnregistered: the env asked for is not in the registry.
	ReasonUnregistered Reason = "unregistered_env"
	// ReasonUnset: the env is empty or absent.
	ReasonUnset Reason = "unset"
	// ReasonTooShort: the value is below the spec's length floor.
	ReasonTooShort Reason = "too_short"
	// ReasonCollision: the value equals the value of an env registered earlier.
	ReasonCollision Reason = "collision"
)

// Error is the refusal returned by Resolve. Its message is built only from env
// names and byte counts — never from a token value.
type Error struct {
	// Env is the env being resolved.
	Env string
	// Reason is why it was refused.
	Reason Reason
	// Other is the colliding env name; set only for ReasonCollision.
	Other string

	msg string
}

func (e *Error) Error() string { return e.msg }

// Specs returns a copy of the registry, in precedence order.
func Specs() []Spec {
	out := make([]Spec, len(registry))
	copy(out, registry)
	return out
}

// Envs returns every registered env name, in precedence order.
func Envs() []string {
	out := make([]string, 0, len(registry))
	for _, spec := range registry {
		out = append(out, spec.Env)
	}
	return out
}

// Registered reports whether env is in the registry.
func Registered(env string) bool {
	_, _, ok := lookup(env)
	return ok
}

// lookup returns the spec for env and its precedence index.
func lookup(env string) (Spec, int, bool) {
	for i, spec := range registry {
		if spec.Env == env {
			return spec, i, true
		}
	}
	return Spec{}, 0, false
}

// yieldsTo reports whether subject gives up its token on a shared value with
// other. otherIsSenior says whether other is registered before subject.
//
// Precedence is the default: the junior yields, the incumbent keeps serving.
// Either side being Mutual makes the pair symmetric instead, so both are
// disabled — see Spec.Mutual for when that is the right choice.
func yieldsTo(subject, other Spec, otherIsSenior bool) bool {
	return otherIsSenior || subject.Mutual || other.Mutual
}

// Yields reports whether resolving subject is refused when its value is shared
// with other — i.e. whether subject is the side that gets disabled.
//
// It exists so a module's own tests can branch on the rule instead of restating
// it. A test that hard-codes "every other env disables me" passes only while
// its subject happens to be registered last, and goes red the moment the
// registry grows; one that asks Yields stays correct through an append and
// through a Spec being marked Mutual.
//
// Unregistered names yield false: Resolve refuses them outright, so there is no
// pair to speak of.
func Yields(subject, other string) bool {
	subjectSpec, subjectIndex, ok := lookup(subject)
	if !ok {
		return false
	}
	otherSpec, otherIndex, ok := lookup(other)
	if !ok || subject == other {
		return false
	}
	return yieldsTo(subjectSpec, otherSpec, otherIndex < subjectIndex)
}

// Resolve loads env and returns its value only when the value can grant
// exactly one capability: it must be set, clear the spec's length floor, and
// differ from the value of every env it yields to — by default the ones
// registered BEFORE it (see "Registration order is a precedence order" in the
// package doc), plus, in either direction, any env marked Mutual.
//
// On refusal it returns ("", *Error) — the empty token is what makes the
// caller's auth middleware fail closed. Callers log the error and carry on;
// they must not fail boot on it (see the package doc).
func Resolve(env string, getenv func(string) string) (string, error) {
	spec, index, ok := lookup(env)
	if !ok {
		return "", &Error{
			Env:    env,
			Reason: ReasonUnregistered,
			msg: fmt.Sprintf("%s is not a registered internal-token env; "+
				"add it to pkg/internaltoken's registry — capability disabled", env),
		}
	}
	if getenv == nil {
		return "", &Error{
			Env:    env,
			Reason: ReasonUnavailable,
			msg:    fmt.Sprintf("%s lookup unavailable; %s disabled", spec.Env, spec.Capability),
		}
	}
	token := getenv(spec.Env)
	if token == "" {
		return "", &Error{
			Env:    env,
			Reason: ReasonUnset,
			msg: fmt.Sprintf("%s not set; %s disabled and will reject all requests",
				spec.Env, spec.Capability),
		}
	}
	// The length gate runs before the collision gate because a value below the
	// floor is unusable on its own terms: "lengthen this secret" is the whole
	// fix, and naming a sibling env alongside it only adds a second thing for
	// the operator to rule out.
	if spec.MinBytes > 0 && len(token) < spec.MinBytes {
		return "", &Error{
			Env:    env,
			Reason: ReasonTooShort,
			msg: fmt.Sprintf("%s must be at least %d bytes; %s disabled",
				spec.Env, spec.MinBytes, spec.Capability),
		}
	}
	for i, other := range registry {
		if i == index {
			continue
		}
		if !yieldsTo(spec, other, i < index) {
			continue
		}
		if token == getenv(other.Env) {
			return "", &Error{
				Env:    env,
				Reason: ReasonCollision,
				Other:  other.Env,
				msg: fmt.Sprintf("%s must differ from %s; %s disabled",
					spec.Env, other.Env, spec.Capability),
			}
		}
	}
	return token, nil
}

// Values returns the non-empty value of every registered env, in precedence
// order.
//
// main.go feeds these into cardactiondispatch.Registry.ValidateNotifyTokenExclusions,
// which is the only place that also sees the *dynamic* per-route notify tokens
// and callback secrets loaded from OCTO_CARD_ACTION_ROUTES. Sourcing the list
// from the registry means a newly registered capability joins that cross-check
// automatically instead of waiting for someone to extend an argument list.
//
// A nil getenv panics rather than returning nothing. Returning an empty slice
// would turn that boot gate — the one check here that ABORTS startup — into a
// silent no-op, which is the single outcome a credential-exclusion gate must
// never have. Same rationale as mustLookupSharedCode in the i18n helpers:
// unrecoverable wiring mistakes fail loudly at boot.
func Values(getenv func(string) string) []string {
	if getenv == nil {
		panic("internaltoken: Values requires a getenv; a nil lookup would silently disable the credential-exclusion gate")
	}
	out := make([]string, 0, len(registry))
	for _, spec := range registry {
		if value := getenv(spec.Env); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// Collision names one env that Resolve disabled because its value duplicates an
// env registered earlier. Env names only — no value is ever carried.
type Collision struct {
	// Senior is the earlier-registered env whose value the junior duplicated.
	Senior string
	// Junior is the env Resolve disabled over the shared value.
	Junior string
	// SeniorServing reports whether the senior itself resolves. It is normally
	// true — that is the whole point of the precedence rule — but a senior can
	// be dark for its own reason (a value below its floor), and a log line that
	// says "this one is still serving" must not say it then.
	SeniorServing bool
}

// Collisions reports every capability Resolve disabled over a shared value, in
// precedence order.
//
// It is derived from Resolve rather than re-deriving the comparison, so the
// boot report cannot contradict what actually happened. Two consequences worth
// stating, because the obvious "compare every pair" implementation gets both
// wrong:
//
//   - A value below its env's floor is refused for length, not collision, so no
//     pair is reported for it. That preserves Resolve's deliberate ordering —
//     "lengthen this secret" is the whole fix, and naming a sibling env
//     alongside it only adds a second thing for the operator to rule out.
//
//   - When three envs share one value, only two lines appear, both naming the
//     first env as the survivor. Reporting all three unordered pairs would name
//     the middle env as a survivor while Resolve has it disabled, and an
//     operator acting on that line rotates the wrong secret.
//
// Resolve already disables each junior on its own; this exists so boot can show
// the whole picture at once, since a standalone "docs capability disabled" line
// leaves the operator to work out which env it duplicated.
//
// A nil getenv panics, for the same reason as Values.
func Collisions(getenv func(string) string) []Collision {
	if getenv == nil {
		panic("internaltoken: Collisions requires a getenv; a nil lookup would silently report no collisions")
	}
	var out []Collision
	for _, spec := range registry {
		_, err := Resolve(spec.Env, getenv)
		var resolveErr *Error
		if !errors.As(err, &resolveErr) || resolveErr.Reason != ReasonCollision {
			continue
		}
		_, seniorErr := Resolve(resolveErr.Other, getenv)
		out = append(out, Collision{
			Senior:        resolveErr.Other,
			Junior:        spec.Env,
			SeniorServing: seniorErr == nil,
		})
	}
	return out
}
