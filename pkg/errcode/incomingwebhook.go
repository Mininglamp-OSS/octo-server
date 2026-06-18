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
// uniform RESPONSE (same code/message/status), so a probe cannot distinguish
// "no such webhook" from "wrong token" by the response body. Timing is only
// best-effort, not constant: not-found returns before any hash, wrong-token
// pays the SHA-256 compare, valid-token+dead-group pays an extra DB round-trip.
// The 128-bit unguessable webhook_id plus per-IP rate limit make timing-based
// enumeration impractical; the response-uniformity is the primary defense.
// See modules/incomingwebhook/api.go pushUnauthorized. Migrated off
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
	// JSON, empty content, malformed rich-text blocks, an unknown msg_type, or a
	// platform-adapter request that cannot be translated (missing X-GitHub-Event
	// header, unsupported WeCom msgtype). The offending stage is surfaced via
	// Details["reason"] (body / json / content / blocks / msg_type / no_event).
	// Note: no_event is the missing-X-GitHub-Event 400; a GitHub event that is
	// merely outside the rendered subset is NOT an error — it answers 200 with
	// skipped=event (see modules/incomingwebhook/adapter_github.go).
	ErrIncomingWebhookPushPayloadInvalid = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_payload_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request payload.",
		SafeDetailKeys: []string{"reason"},
	})

	// ErrIncomingWebhookPushPayloadTooLarge (413) — body exceeds the configured
	// per-shape byte cap: native/wecom use DM_INCOMINGWEBHOOK_MAX_BYTES (8KiB
	// default, caller-authored bodies), GitHub events use the wider
	// DM_INCOMINGWEBHOOK_GITHUB_MAX_BYTES (1MiB default, clamped to 25MiB; the
	// platform-generated event JSON the sender cannot shorten). Rich-text payloads
	// also 413 here when finalized output exceeds the richtext size limit.
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

	// ErrIncomingWebhookPushDisabled (404) — the incoming-webhook feature is
	// globally disabled via system_setting incomingwebhook.enabled=0. Returned
	// for every push while disabled; it is a global state (not per-webhook), so
	// a uniform 404 does not leak whether any specific webhook exists.
	ErrIncomingWebhookPushDisabled = register(codes.Code{
		ID:             "err.server.incomingwebhook.push_disabled",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Not found.",
	})
)

// err.server.incomingwebhook.mgmt_* — management-endpoint error codes
// (create / list / update / delete / regenerate in modules/incomingwebhook/
// api.go). These are authenticated, admin-only endpoints with no legacy
// clients, so — like the push path — they are rendered via
// httperr.ResponseErrorLWithStatus and keep their real semantic HTTP status
// (403/404/409/400/500). This replaces the legacy raw-string
// c.ResponseError(errors.New(...)) pattern (#246 follow-up).
var (
	// ErrIncomingWebhookForbidden (403) — caller lacks permission: not an
	// (internal, active) member of the group, or a plain member/bot touching a
	// webhook created by someone else. Owners/admins manage any webhook;
	// members and bots manage only their own. One generic code for every
	// permission failure — the precise reason is not surfaced.
	ErrIncomingWebhookForbidden = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to manage this webhook.",
	})

	// ErrIncomingWebhookRequestInvalid (400) — malformed body or invalid field
	// (blank/over-long name, status not in {0,1}). The offending field is
	// surfaced via Details["reason"] (body / name / status).
	ErrIncomingWebhookRequestInvalid = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"reason"},
	})

	// ErrIncomingWebhookGroupNotFound (404) — group does not exist or is no
	// longer Normal (disbanded/disabled). Blocks create / enable / regenerate
	// from reviving webhooks on a dead group.
	ErrIncomingWebhookGroupNotFound = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_group_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The group does not exist or has been disbanded.",
	})

	// ErrIncomingWebhookNotFound (404) — webhook does not exist or does not
	// belong to the group in the path (cross-group guard).
	ErrIncomingWebhookNotFound = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The webhook does not exist.",
	})

	// ErrIncomingWebhookQuotaExceeded (409) — the group already holds the
	// maximum number of webhooks. Params["max"] carries the configured cap.
	ErrIncomingWebhookQuotaExceeded = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_quota_exceeded",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Each group allows at most {{.max}} webhooks.",
	})

	// ErrIncomingWebhookCreatorQuotaExceeded (409) — the calling member/bot has
	// reached its per-creator cap in this group (owners/admins are exempt and
	// only bounded by the group cap). Params["max"] carries the configured cap.
	ErrIncomingWebhookCreatorQuotaExceeded = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_creator_quota_exceeded",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You can create at most {{.max}} webhooks in this group.",
	})

	// ErrIncomingWebhookCreatorLeft (409) — the webhook's creator is no longer
	// an (internal, active) member of the group, so operations that would make
	// or keep it pushable (enable / regenerate / test push) are refused. The
	// webhook can still be deleted; pushes are lazily disabled by the push
	// path's creator-membership gate. Recovery: the creator rejoins the group,
	// after which the creator or an admin can re-enable it — the message must
	// state that path and offer deletion only as the cleanup option (PR #340
	// review, Jerry-Xin: do not tell users delete-and-recreate is the only way).
	ErrIncomingWebhookCreatorLeft = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_creator_left",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The webhook's creator has left the group, so it cannot be enabled. It can be re-enabled after the creator rejoins, or deleted and recreated by someone in the group.",
	})

	// ErrIncomingWebhookQueryFailed (500, Internal) — a read (group / webhook /
	// list / permission) failed; the underlying error is logged, not surfaced.
	ErrIncomingWebhookQueryFailed = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query webhook information.",
		Internal:       true,
	})

	// ErrIncomingWebhookOperationFailed (500, Internal) — a write
	// (create / update / delete / regenerate / token generation) failed; the
	// underlying error is logged, not surfaced.
	ErrIncomingWebhookOperationFailed = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_operation_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "The operation failed. Please try again later.",
		Internal:       true,
	})

	// ErrIncomingWebhookDisabled (403) — the incoming-webhook feature is globally
	// disabled via system_setting incomingwebhook.enabled=0. Returned for every
	// management write (create / update / delete / regenerate) while disabled;
	// list (read) stays available so operators can still inspect existing config.
	ErrIncomingWebhookDisabled = register(codes.Code{
		ID:             "err.server.incomingwebhook.mgmt_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The incoming webhook feature is currently disabled.",
	})
)
