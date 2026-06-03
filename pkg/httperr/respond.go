package httperr

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	"go.uber.org/zap"
)

// ResponseErrorL is the business-facing localized error facade.
//
// It validates the code, separates Params from Details, preserves the legacy
// HTTP/body status=400 compatibility path (D14), and delegates
// translation/envelope rendering to the injected wkhttp.ErrorRenderer.
//
// ResponseErrorL writes the response but does not abort the gin chain. Handlers
// must return immediately after calling it, or call c.Abort() when used inside
// middleware.
func ResponseErrorL(c *wkhttp.Context, code codes.Code, params i18n.Params, details i18n.Details) {
	respondL(c, code, params, details, false)
}

// ResponseErrorLWithStatus is the localized error facade for endpoints that
// intentionally keep their real HTTP transport status instead of the legacy
// compatibility 400 (D14).
//
// Use this ONLY for endpoints that have NO legacy clients depending on the
// fixed-400 behavior. Current consumers:
//   - the OIDC self-service bind flow (modules/oidc/api_bind.go) — always
//     returned semantic codes (400/401/409/410/422/429/503);
//   - the Octo-link Bot bind/unbind endpoints (modules/botfather/api_user.go,
//     POST/DELETE /v1/user/bots/:bot_id/bind) — new endpoints that must return
//     real REST status (404/409/…) so external Agents branch on the wire code.
//
// The body envelope is byte-for-byte identical to ResponseErrorL; only the
// transport status differs — here it equals the code's canonical HTTPStatus, so
// the wire status and error.http_status agree. For every other (legacy-bearing)
// endpoint use ResponseErrorL, which keeps wire=400 during the compatibility
// window; the fleet-wide switch to real status codes happens once in Phase 4.
func ResponseErrorLWithStatus(c *wkhttp.Context, code codes.Code, params i18n.Params, details i18n.Details) {
	respondL(c, code, params, details, true)
}

// respondL is the shared implementation behind ResponseErrorL /
// ResponseErrorLWithStatus. useSemanticStatus selects the wire transport
// status: false → legacy compatibility 400 (D14); true → the code's canonical
// HTTPStatus. The body envelope is identical in both cases.
func respondL(c *wkhttp.Context, code codes.Code, params i18n.Params, details i18n.Details, useSemanticStatus bool) {
	if c == nil {
		return
	}

	registered, ok := codes.Lookup(code.ID)
	if !ok {
		log.Error("unregistered i18n error code", zap.String("code", code.ID), zap.String("path", c.FullPath()))
		registered, _ = codes.Lookup("err.shared.internal")
	}

	transportStatus := http.StatusBadRequest
	if useSemanticStatus {
		transportStatus = registered.HTTPStatus
	}

	c.RenderError(wkhttp.ErrorSpec{
		Code:            registered.ID,
		DefaultMessage:  registered.DefaultMessage,
		TransportStatus: transportStatus,
		SemanticStatus:  registered.HTTPStatus,
		Params:          params,
		Details:         details.FilterBy(registered),
		Internal:        registered.Internal,
	})
}
