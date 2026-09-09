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
// Both rules now come from pkg/internaltoken rather than from branches here:
//
//   - the intra-module tie-break (OCTO_DOCS_NOTIFY_TOKEN yields to
//     NOTIFY_INTERNAL_TOKEN) is the registry's precedence rule, in the same
//     direction this file used to hand-roll it;
//   - the exclusion against OCTO_MARKETPLACE_INTERNAL_TOKEN is that Spec's
//     Mutual flag, which disables BOTH sides — the mirror-image behaviour #827
//     introduced, expressed once instead of in four modules.
//
// The pre-existing pairs against OCTO_DOCS_BOT_MENTION_TOKEN and
// OCTO_DRIVE_INTERNAL_TOKEN keep behaving exactly as they did: both are
// registered after these two envs and are not Mutual, so they are the side that
// yields. Switching a currently-serving deployment's notify path off is still
// not something this file does.
//
// Comparison is byte-exact on the raw env values, in pkg/internaltoken as it
// was here. No normalization is applied anywhere: the tokens returned here are what
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
)

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

	return token, docsToken, warnings, bootErrors
}
