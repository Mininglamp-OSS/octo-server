package incomingwebhook

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// push-path error responders. The push endpoint is unauthenticated (token in
// URL); all error responses go through the i18n facade instead of raw
// c.AbortWithStatusJSON, satisfying the D23 lint gate. ResponseErrorLWithStatus
// preserves the real HTTP status (401/429/400/413/502) — webhook senders are
// machines that key off the status code, so it must stay truthful. Each helper
// Aborts so no later handler runs (mirrors the previous AbortWithStatusJSON).

// pushUnauthorized returns the uniform 401 for EVERY auth failure on the push
// path (missing/disabled webhook, bad token, disbanded group). It must stay a
// single code/message: differentiating the reason would leak webhook existence
// to a probe scanning tokens.
func pushUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushUnauthorized, nil, nil)
	c.Abort()
}

// pushRateLimited returns 429 when a push limiter (local floor, per-IP failure
// gate, or per-webhook bucket) rejects. It sets a conservative Retry-After so
// machine senders back off instead of hot-looping; the limiters refill within
// ~1s at their configured rates, so a fixed 1s hint is a safe lower bound. (The
// per-IP request limiter, StrictIPRateLimitMiddleware, sets its own precise
// Retry-After / X-RateLimit headers and never reaches here.)
func pushRateLimited(c *wkhttp.Context) {
	c.Header("Retry-After", "1")
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushRateLimited, nil, nil)
	c.Abort()
}

// pushPayloadInvalid returns 400 for unreadable body / malformed JSON / empty
// content / malformed rich-text blocks / unknown msg_type / untranslatable
// platform-adapter request; reason ∈ {body, json, content, blocks, msg_type,
// event} is surfaced via the safe-listed Details key so callers can tell what
// to fix.
func pushPayloadInvalid(c *wkhttp.Context, reason string) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushPayloadInvalid, nil, i18n.Details{"reason": reason})
	c.Abort()
}

// pushPayloadTooLarge returns 413 when the body exceeds the configured cap.
func pushPayloadTooLarge(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushPayloadTooLarge, nil, nil)
	c.Abort()
}

// pushDeliveryFailed returns 502 when the downstream SendMessage fails. The
// code is Internal=true, so the renderer emits a generic message and the real
// error is only logged by the caller.
func pushDeliveryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushDeliveryFailed, nil, nil)
	c.Abort()
}

// pushDisabled returns 404 when the feature is globally disabled
// (system_setting incomingwebhook.enabled=0). Uniform across all pushes — a
// global state, not a per-webhook signal, so it does not leak webhook existence.
func pushDisabled(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushDisabled, nil, nil)
	c.Abort()
}

// ============================================================
// management-path error responders
//
// The admin-only management endpoints (create / list / update / delete /
// regenerate) also use ResponseErrorLWithStatus and keep their real semantic
// HTTP status — they are a new feature with no legacy clients keyed to the
// fixed-400 ResponseErrorL. These replace the legacy raw-string
// c.ResponseError(errors.New(...)) pattern (#246). Handlers must return
// immediately after calling one; no c.Abort() is needed at handler level.
// ============================================================

// mgmtForbidden returns 403 — caller is neither owner nor admin.
func mgmtForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookForbidden, nil, nil)
}

// mgmtFeatureDisabled returns 403 when the feature is globally disabled
// (system_setting incomingwebhook.enabled=0). Gates every management write
// (create/update/delete/regenerate); list (read) stays open.
//
// Unlike the other mgmt responders (which are called from inside a handler that
// returns immediately after), this one runs from the requireMgmtEnabled
// MIDDLEWARE, so it MUST c.Abort() — a bare return only exits the middleware
// closure and Gin would still invoke the downstream write handler, executing
// the mutation after the 403 was written. Symmetric with pushDisabled.
func mgmtFeatureDisabled(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookDisabled, nil, nil)
	c.Abort()
}

// mgmtRequestInvalid returns 400 for malformed body / invalid field; reason ∈
// {body, name, status} is surfaced via the safe-listed Details key.
func mgmtRequestInvalid(c *wkhttp.Context, reason string) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookRequestInvalid, nil, i18n.Details{"reason": reason})
}

// mgmtGroupNotFound returns 404 — group missing or not Normal (disbanded).
func mgmtGroupNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookGroupNotFound, nil, nil)
}

// mgmtNotFound returns 404 — webhook missing or cross-group.
func mgmtNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookNotFound, nil, nil)
}

// mgmtQuotaExceeded returns 409 — per-group webhook cap reached; max carries
// the configured limit for the message.
func mgmtQuotaExceeded(c *wkhttp.Context, max int) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookQuotaExceeded, i18n.Params{"max": max}, nil)
}

// mgmtQueryFailed returns 500 (Internal) — a read failed; real error logged.
func mgmtQueryFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookQueryFailed, nil, nil)
}

// mgmtOperationFailed returns 500 (Internal) — a write failed; real error logged.
func mgmtOperationFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookOperationFailed, nil, nil)
}
