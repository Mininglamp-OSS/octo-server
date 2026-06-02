package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.oidc.* — modules/oidc self-service bind error codes
// (api_bind.go). DefaultMessage holds the en-US source (D4); the zh-CN runtime
// translation lives in pkg/i18n/locales/active.zh-CN.toml.
//
// These codes are rendered via httperr.ResponseErrorLWithStatus (NOT the
// fixed-400 ResponseErrorL): the bind endpoints are a recent feature with no
// legacy clients, and have always returned semantic status codes
// (400/401/409/410/422/429/503). The wire status therefore equals HTTPStatus.
//
// Anti-enumeration: ErrBindAuthRejected (wrong password / bad OTP / phone not
// matched) and the verify→confirm TOCTOU rejection all map to the single
// ErrOIDCBindInvalidCredentials (401) with a generic message — the specific
// reason is logged via zap only, never surfaced, so the response cannot be used
// to probe "account exists vs password wrong".
//
// Only the 503 code is 5xx and therefore Internal=true (its message is a generic
// placeholder; clients identify the transient condition from error.http_status).
// Genuine internal failures (the default branch of each handle*Err) reuse
// err.shared.internal rather than a bespoke oidc code.
var (
	// ---- transient infrastructure (503) -------------------------------------

	// ErrOIDCBindServiceUnavailable is returned when the bind sub-service failed
	// to initialise (Discovery failed → o.bind is nil) but the routes are still
	// mounted. Transient: ops can recover by fixing Discovery / restarting.
	//
	// NOTE: Internal=true (5xx hygiene) means the renderer emits the shared
	// internal-error copy, not this DefaultMessage — the specific text and its
	// zh-CN translation are intentionally unreachable on the wire. They are kept
	// to satisfy Register and to document intent; clients distinguish the
	// transient 503 via error.http_status, not the message.
	ErrOIDCBindServiceUnavailable = register(codes.Code{
		ID:             "err.server.oidc.bind_service_unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "The account binding service is temporarily unavailable. Please try again later.",
		Internal:       true,
	})

	// ---- validation (400) ---------------------------------------------------

	// ErrOIDCBindRequestInvalid is the catch-all for malformed bind input:
	// BindJSON failure, missing/blank token, bad authcode format, or missing
	// identifier/password/code fields.
	ErrOIDCBindRequestInvalid = register(codes.Code{
		ID:             "err.server.oidc.bind_request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
	})
	// ErrOIDCBindSMSUnavailable maps ErrBindNoPhone: the SSO claims carry no
	// verified phone, so SMS verification cannot proceed — the client should
	// switch to the password method rather than retry.
	ErrOIDCBindSMSUnavailable = register(codes.Code{
		ID:             "err.server.oidc.bind_sms_unavailable",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "SMS verification is not available for this account.",
	})
	// ErrOIDCBindMethodDisabled maps ErrBindMethodDisabled: the verification
	// method was turned off via OCTO_OIDC_BIND_METHODS — the client should not
	// retry it.
	ErrOIDCBindMethodDisabled = register(codes.Code{
		ID:             "err.server.oidc.bind_method_disabled",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This verification method is currently disabled.",
	})

	// ---- auth / anti-enumeration (401) --------------------------------------

	// ErrOIDCBindInvalidCredentials maps ErrBindAuthRejected (wrong password /
	// bad OTP / phone not matched) and the confirm-stage TOCTOU rejection. A
	// single generic 401 across all causes prevents account enumeration.
	ErrOIDCBindInvalidCredentials = register(codes.Code{
		ID:             "err.server.oidc.bind_invalid_credentials",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Verification failed. Please check your details and try again.",
	})
	// ErrOIDCBindVerifyRequired maps ErrBindStatusConflict on the confirm path:
	// the session is not yet verified (user skipped the second factor or a
	// concurrent confirm raced).
	ErrOIDCBindVerifyRequired = register(codes.Code{
		ID:             "err.server.oidc.bind_verify_required",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Please complete verification before confirming.",
	})

	// ---- conflict (409) -----------------------------------------------------

	// ErrOIDCBindAlreadyVerified maps ErrBindStatusConflict on the verify path:
	// the session is already verified; the client should skip ahead to confirm.
	ErrOIDCBindAlreadyVerified = register(codes.Code{
		ID:             "err.server.oidc.bind_already_verified",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This binding session has already been verified.",
	})
	// ErrOIDCBindStatusConflict maps ErrBindStatusConflict on the create path:
	// the session state does not allow account creation.
	ErrOIDCBindStatusConflict = register(codes.Code{
		ID:             "err.server.oidc.bind_status_conflict",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The binding session is in a conflicting state.",
	})
	// ErrOIDCBindAlreadyBound maps ErrBindAlreadyBound: the identity row already
	// exists (a retry after a successful bind, or a create race). The client is
	// guided back to the OIDC sign-in entry point.
	ErrOIDCBindAlreadyBound = register(codes.Code{
		ID:             "err.server.oidc.bind_already_bound",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This identity is already bound. Please sign in via OIDC to continue.",
	})
	// ErrOIDCBindConflictNeedManual maps ErrBindConflictNeedManual /
	// ErrBindCreateConflictNeedManual: the claims match multiple accounts, which
	// the self-service flow cannot resolve — route to admin/manual handling.
	ErrOIDCBindConflictNeedManual = register(codes.Code{
		ID:             "err.server.oidc.bind_conflict_need_manual",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Multiple accounts match; manual resolution is required. Please contact support.",
	})

	// ---- gone (410) ---------------------------------------------------------

	// ErrOIDCBindTokenInvalid maps ErrBindNotFound: the single-use 5-minute
	// bind_token has expired or was already consumed. The client must restart
	// the OIDC flow.
	ErrOIDCBindTokenInvalid = register(codes.Code{
		ID:             "err.server.oidc.bind_token_invalid",
		HTTPStatus:     http.StatusGone,
		DefaultMessage: "The binding session has expired or is invalid. Please start over.",
	})

	// ---- unprocessable (422) ------------------------------------------------

	// ErrOIDCBindClaimsIncomplete maps ErrBindCreateClaimsIncomplete: the SSO
	// claims carry neither a verified email nor a verified phone, so /bind/create
	// cannot provision an account.
	ErrOIDCBindClaimsIncomplete = register(codes.Code{
		ID:             "err.server.oidc.bind_claims_incomplete",
		HTTPStatus:     http.StatusUnprocessableEntity,
		DefaultMessage: "The identity provider did not supply the account details required to create an account.",
	})
)
