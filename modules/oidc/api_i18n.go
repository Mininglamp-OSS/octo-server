package oidc

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// respond helpers for the OIDC self-service bind flow (api_bind.go).
//
// Every bind error renders through respondBindError, which uses
// httperr.ResponseErrorLWithStatus — the bind endpoints keep their real HTTP
// status (400/401/409/410/422/429/503) instead of the legacy compatibility 400,
// because they are a recent feature with no clients depending on fixed-400.
//
// The OAuth2/OIDC protocol endpoints in api.go (authorize/callback/logout) are
// intentionally NOT migrated: they are consumed by the browser redirect flow,
// not the dmwork front-end, and keep their raw errMsg responses.

// shared codes reused by the bind flow, resolved once at init. A missing
// registration panics loudly here rather than silently degrading to an empty
// envelope at request time.
var (
	codeSharedRateLimited = mustLookupSharedCode("err.shared.rate.limited")
	codeSharedInternal    = mustLookupSharedCode("err.shared.internal")
)

func mustLookupSharedCode(id string) codes.Code {
	c, ok := codes.Lookup(id)
	if !ok {
		panic("modules/oidc: shared code not registered: " + id)
	}
	return c
}

// respondBindError renders a localized bind error while keeping the code's real
// HTTP transport status, then aborts the gin chain — matching the prior
// c.AbortWithStatusJSON(errMsg(...)) semantics so callers can return immediately.
func respondBindError(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorLWithStatus(c, code, nil, nil)
	c.Abort()
}
