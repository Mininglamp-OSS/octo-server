package notify

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-server/pkg/internaltoken"
)

// Internal-token resolution for modules/notify.
//
// # What this file adds, and what it deliberately leaves alone
//
// The two notify credentials (NOTIFY_INTERNAL_TOKEN, OCTO_DOCS_NOTIFY_TOKEN)
// and the intra-module tie-break between them are pre-existing behaviour, moved
// out of New() unchanged so the rules are unit-testable without mutating
// process env — the same shape modules/internal_resolve, modules/bot_mention
// and modules/space already use.
//
// The one NEW rule is the exclusion against OCTO_MARKETPLACE_INTERNAL_TOKEN,
// the single fixed internal-token env this change introduces
// (modules/space.MarketplaceInternalTokenEnv). modules/space refuses to enable
// the Space role lookup on a value shared with either notify credential; this
// is the mirror-image half, so a deployment that sets one value for both fails
// BOTH capabilities closed instead of picking an arbitrary winner and leaving
// the leaked value serving here.
//
// Scope note: the exclusion set is deliberately NOT extended to the other
// pre-existing fixed internal-token envs (OCTO_DOCS_BOT_MENTION_TOKEN,
// OCTO_DRIVE_INTERNAL_TOKEN). Those pairs predate this change; switching a
// currently-serving deployment's notify path off is not something a marketplace
// feature gets to do as a side effect.
//
// Comparison is byte-exact on the raw env values, matching modules/space,
// modules/bot_mention and modules/internal_resolve. No normalization is applied
// anywhere in this file: the tokens returned here are what
// internalAuthMiddleware compares against the request header, and
// NOTIFY_INTERNAL_TOKEN / OCTO_DOCS_NOTIFY_TOKEN are pre-existing production
// credentials whose accepted bytes must not change. Trimming on one side only
// would also reintroduce the arbitrary winner this exists to prevent, since the
// sibling modules compare raw.
//
// These resolvers can only ever disable THEMSELVES on a collision — the process
// still boots, with a logged error, and the affected capability rejects every
// request. That is the actual behaviour, and docs/space-internal-role-api.md
// §5.1 describes it as such.

const (
	// The two notify credentials, sourced from the shared registry so their
	// spellings cannot drift from the entry the collision check compares.
	notifyInternalTokenEnv     = internaltoken.NotifyInternalTokenEnv
	docsNotifyInternalTokenEnv = internaltoken.DocsNotifyTokenEnv

	// marketplaceInternalTokenEnvForExclusion is
	// modules/space.MarketplaceInternalTokenEnv. Duplicated as a literal rather
	// than imported, matching modules/internal_resolve/config.go and
	// modules/bot_mention/config.go: no module should take a production
	// dependency on another just to learn a string. The spelling is pinned
	// against the owning package by a test.
	marketplaceInternalTokenEnvForExclusion = "OCTO_MARKETPLACE_INTERNAL_TOKEN"
)

// collidesWithForeignFixedToken reports the foreign env name a non-empty token
// collides with, or "" when it is clean. An empty token never "collides":
// unset already means the capability is disabled, and reporting a collision
// between two unset envs would produce a confusing boot error.
func collidesWithForeignFixedToken(token string, getenv func(string) string) string {
	if token == "" || getenv == nil {
		return ""
	}
	if getenv(marketplaceInternalTokenEnvForExclusion) == token {
		return marketplaceInternalTokenEnvForExclusion
	}
	return ""
}

// resolveInternalTokens loads NOTIFY_INTERNAL_TOKEN and OCTO_DOCS_NOTIFY_TOKEN
// and returns them alongside human-readable, logger-safe diagnostics (never
// containing token values). A token that collides comes back empty, which makes
// internalAuthMiddleware reject it (fail-closed).
//
// Split out of New() as a pure function so the rules are unit-testable without
// mutating process env; the warning/error strings and the intra-module
// tie-break are carried over verbatim.
func resolveInternalTokens(getenv func(string) string) (token, docsToken string, warnings, bootErrors []string) {
	if getenv == nil {
		return "", "", nil, []string{"internal token lookup unavailable; notify internal API disabled"}
	}

	// resolve runs one env through the shared registry, which enforces the
	// length floor and compares the value against every env registered BEFORE
	// it. For NOTIFY_INTERNAL_TOKEN (registered first) that is nothing; for
	// OCTO_DOCS_NOTIFY_TOKEN it is the legacy token — i.e. exactly the
	// intra-module tie-break this function used to run by hand, in the same
	// direction. An unset env is a normal deployment shape and reports as a
	// warning; a collision or an undersized value is an operator mistake that
	// silently turns an ingress off, and reports as a boot error.
	resolve := func(env string) string {
		value, err := internaltoken.Resolve(env, getenv)
		if err == nil {
			return value
		}
		var resolveErr *internaltoken.Error
		if errors.As(err, &resolveErr) && resolveErr.Reason == internaltoken.ReasonUnset {
			warnings = append(warnings, err.Error())
			return ""
		}
		bootErrors = append(bootErrors, err.Error())
		return ""
	}

	token = resolve(notifyInternalTokenEnv)
	docsToken = resolve(docsNotifyInternalTokenEnv)

	// Mirror-image half for OCTO_MARKETPLACE_INTERNAL_TOKEN (#827). That env is
	// not in the registry yet, and its pairs were deliberately made symmetric
	// rather than precedence-ordered, so both halves stay explicit until the
	// follow-up absorbs it.
	if env := collidesWithForeignFixedToken(token, getenv); env != "" {
		bootErrors = append(bootErrors, notifyInternalTokenEnv+" must differ from "+env+
			"; legacy notify capability disabled")
		token = ""
	}
	if env := collidesWithForeignFixedToken(docsToken, getenv); env != "" {
		bootErrors = append(bootErrors, docsNotifyInternalTokenEnv+" must differ from "+env+
			"; docs notify capability disabled")
		docsToken = ""
	}
	return token, docsToken, warnings, bootErrors
}
