package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.incomingwebhook.* — modules/incomingwebhook push-endpoint error
// codes (api.go push path). DefaultMessage holds the en-US source; the zh-CN
// runtime translation lives in pkg/i18n/locales/active.zh-CN.toml.
//
// The push endpoint is an unauthenticated, token-in-URL surface. Every
// authentication failure (missing/disabled webhook, bad token, disbanded
// group) intentionally collapses to the SINGLE push_unauthorized code with a
// uniform message, so a probe cannot tell "no such webhook" from "wrong token"
// — see modules/incomingwebhook/api.go respondPushUnauthorized. Migrated off
// the raw c.AbortWithStatusJSON pattern to satisfy the D23 i18n lint gate
// (PR Mininglamp-OSS/octo-server#31, yujiawei review #3).
var (
	// ErrIncomingWebhookPushUnauthorized (401) — uniform anti-probe response for
	// every auth failure on the push path; never differentiate the reason.
	ErrIncomingWebhookPushUnauthorized = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_unauthorized",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Unauthorized.",
	})

	// ErrIncomingWebhookPushRateLimited (429) — the per-webhook token bucket
	// rejected this request.
	ErrIncomingWebhookPushRateLimited = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_rate_limited",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "Too many requests. Please retry later.",
	})

	// ErrIncomingWebhookPushPayloadInvalid (400) — unreadable body, malformed
	// JSON, or empty content. The offending stage is surfaced via
	// Details["reason"] (body / json / content).
	ErrIncomingWebhookPushPayloadInvalid = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_payload_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request payload.",
		SafeDetailKeys: []string{"reason"},
	})

	// ErrIncomingWebhookPushPayloadTooLarge (413) — body exceeds the configured
	// byte cap (DM_INCOMINGWEBHOOK_MAX_BYTES).
	ErrIncomingWebhookPushPayloadTooLarge = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_payload_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "Request payload is too large.",
	})

	// ErrIncomingWebhookPushDeliveryFailed (502, Internal) — the downstream
	// SendMessage failed; the underlying error is logged, not surfaced.
	ErrIncomingWebhookPushDeliveryFailed = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_delivery_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Failed to deliver the webhook message.",
		Internal:       true,
	})
)
