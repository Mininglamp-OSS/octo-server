package internal_membership

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// respond helpers for modules/internal_membership.
//
// All three reuse the shared err.shared.* codes rather than registering
// module-specific ones, and that is a security choice rather than laziness. The
// error surface a peer service sees must not distinguish WHY a call was refused
// any more finely than it needs to act: a per-reason code here would tell an
// unauthenticated caller whether a token is unset versus wrong, or whether a
// project exists versus lives in another Space. Shared codes keep the
// distinctions in the logs, where they belong.
//
// These use ResponseErrorLWithStatus (the code's real HTTP status) rather than
// modules/project's ResponseErrorL (pinned 400). This module is new, has no
// client depending on a fixed 400, and every sibling /v1/internal endpoint
// (modules/internal_resolve, modules/bot_mention) already answers with real
// statuses — a peer service should read 401 as 401.

// respondUnauthorized is the single answer to every X-Internal-Token failure.
//
// Deliberately identical whether the token is unset, malformed or simply wrong.
// The Loop integration document specifies 503 for "not configured" and 401 for
// "bad token"; that split is not implemented here on purpose, because it lets
// an unauthenticated caller probe deployment state. Operators get the
// distinction from the startup log line that names the misconfigured env, which
// is where it is actionable.
func respondUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
}

// respondInvalidParam emits the shared 400 with an optional `field` detail.
func respondInvalidParam(c *wkhttp.Context, field string) {
	var details i18n.Details
	if field != "" {
		details = i18n.Details{"field": field}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, details)
}

// respondInternal wraps any downstream failure. Kept generic so query text,
// DSN or row contents never reach a peer service.
//
// A failure here must never be mistaken for a truthful negative answer. Both
// handlers return this instead of an empty result set for exactly that reason:
// the consumer's contract is to fail closed on a non-200, and an empty 200
// would read as "nobody is a member".
func respondInternal(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
}
