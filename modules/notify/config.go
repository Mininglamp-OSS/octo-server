package notify

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
	notifyInternalTokenEnv     = "NOTIFY_INTERNAL_TOKEN"
	docsNotifyInternalTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"

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
	token = getenv(notifyInternalTokenEnv)
	docsToken = getenv(docsNotifyInternalTokenEnv)

	if token == "" {
		warnings = append(warnings,
			notifyInternalTokenEnv+" not set — internal API will reject all requests")
	}
	if docsToken == "" {
		warnings = append(warnings,
			docsNotifyInternalTokenEnv+" not set — docs notification requests will be rejected")
	}

	// Pre-existing intra-module tie-break, unchanged. Runs first so the docs
	// token is already cleared before the foreign check looks at it.
	if token != "" && docsToken == token {
		bootErrors = append(bootErrors, docsNotifyInternalTokenEnv+" must differ from "+
			notifyInternalTokenEnv+"; docs capability disabled")
		docsToken = ""
	}

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
