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

// pushRateLimited returns 429 when the per-webhook token bucket rejects.
func pushRateLimited(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrIncomingWebhookPushRateLimited, nil, nil)
	c.Abort()
}

// pushPayloadInvalid returns 400 for unreadable body / malformed JSON / empty
// content; reason ∈ {body, json, content} is surfaced via the safe-listed
// Details key so callers can tell what to fix.
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
