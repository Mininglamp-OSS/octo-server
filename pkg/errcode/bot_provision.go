package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.bot_provision.* — modules/bot_provision business error codes.
// bot_provision is the cross-service "open an account for a daemon" surface:
//   - POST /v1/bot/mint        — web/session caller mints a bot OBO.
//   - GET  /v1/bot/:uid/token  — daemon (api_key Bearer) fetches a bot_token.
//
// Wire status: the mint validation sites historically returned the legacy
// compatibility 400 via c.ResponseError, so they are rendered with
// httperr.ResponseErrorL (wire 400, body error.http_status = the code's status).
// The sites that returned a real semantic status (401/403/404 via
// c.ResponseErrorWithStatus) are rendered with httperr.ResponseErrorLWithStatus
// to PRESERVE that wire status, which the daemon branches on.
//
// Anti-enumeration (CLAUDE.md): the daemon token endpoint's credential failures
// (missing/empty Bearer, unresolvable api_key) ALL collapse to a single
// ErrBotProvisionAuthFailed 401 — the specific reason is logged, never returned,
// so a caller cannot probe which factor was wrong. The mint endpoint's
// defensive "no session uid" guard reuses the shared auth-required code.
//
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrBotProvisionRequestInvalid is the catch-all for malformed/missing mint
	// request input: BindJSON failure (field omitted) and the required-field
	// guards (field = "display_name" / "space_id"). The offending field is
	// surfaced via Details when identifiable; the raw parse error is logged.
	ErrBotProvisionRequestInvalid = register(codes.Code{
		ID:             "err.server.bot_provision.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})

	// ---- auth (401, anti-enumeration) ----------------------------------------

	// ErrBotProvisionAuthFailed is the SINGLE anti-enumeration code for the
	// daemon token endpoint's credential failures: missing Authorization header,
	// empty Bearer token, and unresolvable api_key ALL collapse here so an
	// external caller cannot probe which factor was wrong. The specific reason is
	// logged, never returned. Rendered via ResponseErrorLWithStatus to PRESERVE
	// the real 401 (the daemon branches on it).
	ErrBotProvisionAuthFailed = register(codes.Code{
		ID:             "err.server.bot_provision.auth_failed",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Authentication failed.",
	})

	// ---- permission (403) ----------------------------------------------------

	// ErrBotProvisionSpaceForbidden covers a mint caller who is not a member of
	// the target space (PR-D.1 #2 guard — a user may not mint a bot into a space
	// they do not belong to). DB errors from the membership check are logged and
	// also map here (the original handler returned a single 403 for any failure).
	ErrBotProvisionSpaceForbidden = register(codes.Code{
		ID:             "err.server.bot_provision.space_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this space.",
	})
	// ErrBotProvisionBotForbidden covers the daemon token endpoint when the
	// caller's api_key uid is not the bot's creator — a clean 403 with no leak of
	// whether the bot exists.
	ErrBotProvisionBotForbidden = register(codes.Code{
		ID:             "err.server.bot_provision.bot_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Not authorized for this bot.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrBotProvisionBotNotFound covers the daemon token endpoint when no bot row
	// (status=1, in the caller's space) yields a token.
	ErrBotProvisionBotNotFound = register(codes.Code{
		ID:             "err.server.bot_provision.bot_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Bot not found.",
	})
)

// Shared codes reused at the call sites (no per-module alias needed — the
// global selectors already exist):
//   - errcode.ErrSharedAuthRequired (401) — mint's defensive missing-session-uid
//     guard (the route sits behind AuthMiddleware, so this is belt-and-suspenders).
//   - errcode.ErrSharedInternal (500, Internal=true) — token gen / MintBotOBO /
//     robot lookup failures; message+details never surface, each call site logs
//     the underlying err with zap before responding.
