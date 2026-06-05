package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.bot_api.* — modules/bot_api business error codes. Unlike the
// dmwork-facing modules, every bot_api endpoint serves EXTERNAL bot adapters /
// integrations authenticated by bf_/app_ tokens (plus the OBO grant-management
// endpoints, which serve the logged-in user). Many sites already returned real
// HTTP status codes that adapters branch on, so those are rendered via
// httperr.ResponseErrorLWithStatus to PRESERVE the wire status rather than the
// D14 fixed-400 path; the legacy c.ResponseError sites stay at wire 400.
//
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never
// surface their message on the wire — callers MUST log the underlying err with
// full context (zap.Error) before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrBotAPIRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, "X 不能为空", invalid id/format, empty payload).
	// The offending field is surfaced via Details when identifiable.
	ErrBotAPIRequestInvalid = register(codes.Code{
		ID:             "err.server.bot_api.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrBotAPILimitExceeded covers a list parameter exceeding its cap
	// (members > 200, message_ids > 100). The cap is surfaced so the client can
	// render a localized hint without hard-coding it.
	ErrBotAPILimitExceeded = register(codes.Code{
		ID:             "err.server.bot_api.limit_exceeded",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The request exceeds the maximum allowed items.",
		SafeDetailKeys: []string{"field", "max"},
	})
	// ErrBotAPIContentTooLarge covers a content body exceeding its byte/char cap
	// (GROUP.md content, voice vocabulary context, OBO persona_prompt). The cap
	// is surfaced via Details; the raw content never is.
	ErrBotAPIContentTooLarge = register(codes.Code{
		ID:             "err.server.bot_api.content_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The content exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"field", "max_size", "max_bytes"},
	})
	// ErrBotAPIFileTooLarge surfaces the upload size cap (in MB) for the legacy
	// (wire-400) upload path.
	ErrBotAPIFileTooLarge = register(codes.Code{
		ID:             "err.server.bot_api.file_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The file exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"max_mb"},
	})
	// ErrBotAPIPayloadTooLarge is the status-preserving (413) variant used by the
	// voice transcribe proxy, whose external client branches on HTTP 413.
	ErrBotAPIPayloadTooLarge = register(codes.Code{
		ID:             "err.server.bot_api.payload_too_large",
		HTTPStatus:     http.StatusRequestEntityTooLarge,
		DefaultMessage: "The uploaded file exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"max_bytes"},
	})
	// ErrBotAPIFileTypeUnsupported covers an unsupported / extension-less upload.
	ErrBotAPIFileTypeUnsupported = register(codes.Code{
		ID:             "err.server.bot_api.file_type_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported file type.",
	})
	// ErrBotAPIMemberNotHuman covers the bot-API rule that only human users may
	// be added to a group through this API (no bot members, creator must not be a
	// bot).
	ErrBotAPIMemberNotHuman = register(codes.Code{
		ID:             "err.server.bot_api.member_not_human",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Only human members can be added through the bot API.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrBotAPIThreadChannelNotAccepted rejects a thread channel id supplied to a
	// group-level GROUP.md endpoint (the caller must use the thread md route).
	ErrBotAPIThreadChannelNotAccepted = register(codes.Code{
		ID:             "err.server.bot_api.thread_channel_not_accepted",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "A thread channel id is not accepted here; use the thread GROUP.md endpoint instead.",
	})
	// ErrBotAPIBotNotProvisioned covers a bot that cannot serve the request
	// because it is not fully provisioned (no owner, or not in any active space)
	// — a configuration gap, surfaced as a 400 to the voice client.
	ErrBotAPIBotNotProvisioned = register(codes.Code{
		ID:             "err.server.bot_api.bot_not_provisioned",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The bot is not fully provisioned for this operation.",
	})
	// ErrBotAPIOBOModeUnsupported covers an OBO grant created/updated with a mode
	// other than the only supported value (auto, v0).
	ErrBotAPIOBOModeUnsupported = register(codes.Code{
		ID:             "err.server.bot_api.obo_mode_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported OBO mode.",
	})
	// ErrBotAPIOBOReservedField rejects an inbound message payload that carries a
	// server-only reserved OBO key (__obo_* / obo_* / actual_sender_uid).
	ErrBotAPIOBOReservedField = register(codes.Code{
		ID:             "err.server.bot_api.obo_reserved_field",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The payload uses reserved OBO fields.",
	})

	// ---- permission / authorization (403) ------------------------------------

	// ErrBotAPINotGroupMember covers the bot-not-a-group-member guard.
	ErrBotAPINotGroupMember = register(codes.Code{
		ID:             "err.server.bot_api.not_group_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not a member of this group.",
	})
	// ErrBotAPINotGroupAdmin covers the bot-not-a-group-admin guard.
	ErrBotAPINotGroupAdmin = register(codes.Code{
		ID:             "err.server.bot_api.not_group_admin",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not an admin of this group.",
	})
	// ErrBotAPINotSpaceMember covers the bot/user-not-a-space-member guard.
	ErrBotAPINotSpaceMember = register(codes.Code{
		ID:             "err.server.bot_api.not_space_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Not a member of this space.",
	})
	// ErrBotAPIAppBotUnsupported covers operations an App Bot may not perform
	// (group operations, voice operations) — App Bots are DM-scoped.
	ErrBotAPIAppBotUnsupported = register(codes.Code{
		ID:             "err.server.bot_api.app_bot_unsupported",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This operation is not supported for app bots.",
	})
	// ErrBotAPIAppBotDMOnly covers the App Bot direct-message-only restriction
	// (App Bots may only access / edit direct message channels).
	ErrBotAPIAppBotDMOnly = register(codes.Code{
		ID:             "err.server.bot_api.app_bot_dm_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "App bots can only operate on direct message channels.",
	})
	// ErrBotAPINotFriend covers the user-bot friend gate (the target user has not
	// added / opted into the bot).
	ErrBotAPINotFriend = register(codes.Code{
		ID:             "err.server.bot_api.not_friend",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not a friend of this user.",
	})
	// ErrBotAPIConversationNotStarted covers the App Bot gate where the user has
	// not started a conversation with the bot yet.
	ErrBotAPIConversationNotStarted = register(codes.Code{
		ID:             "err.server.bot_api.conversation_not_started",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The user has not started a conversation with this bot.",
	})
	// ErrBotAPIMessageEditForbidden covers the bot-message edit guard (a bot may
	// only edit messages it sent).
	ErrBotAPIMessageEditForbidden = register(codes.Code{
		ID:             "err.server.bot_api.message_edit_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You can only edit messages you sent.",
	})
	// ErrBotAPIOBONotAuthorized covers a send/typing OBO request lacking an
	// active grant or per-channel scope (ErrOBONotAuthorized).
	ErrBotAPIOBONotAuthorized = register(codes.Code{
		ID:             "err.server.bot_api.obo_not_authorized",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Not authorized to act on behalf of this user.",
	})
	// ErrBotAPIBotUnavailable covers an App Bot that is not published (status!=1)
	// and therefore may not serve API requests. Status-preserving (403).
	ErrBotAPIBotUnavailable = register(codes.Code{
		ID:             "err.server.bot_api.bot_unavailable",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not available.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrBotAPIGroupNotFound covers a missing target group.
	ErrBotAPIGroupNotFound = register(codes.Code{
		ID:             "err.server.bot_api.group_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Group not found.",
	})
	// ErrBotAPIMessageNotFound covers a missing target message on edit.
	ErrBotAPIMessageNotFound = register(codes.Code{
		ID:             "err.server.bot_api.message_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Message not found.",
	})
	// ErrBotAPIUserNotFound covers a missing target user (group member lookup,
	// command target).
	ErrBotAPIUserNotFound = register(codes.Code{
		ID:             "err.server.bot_api.user_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "User not found.",
	})
	// ErrBotAPIBotNotRegistered covers an OBO grantee uid that is not a
	// registered bot (existence-leak-safe 404; ownership mismatch also maps here).
	ErrBotAPIBotNotRegistered = register(codes.Code{
		ID:             "err.server.bot_api.bot_not_registered",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The grantee is not a registered bot.",
	})
	// ErrBotAPIOBOGrantNotFound covers a missing / inactive OBO grant.
	ErrBotAPIOBOGrantNotFound = register(codes.Code{
		ID:             "err.server.bot_api.obo_grant_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "OBO grant not found.",
	})
	// ErrBotAPIOBOScopeNotFound covers a missing OBO per-channel scope.
	ErrBotAPIOBOScopeNotFound = register(codes.Code{
		ID:             "err.server.bot_api.obo_scope_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "OBO scope not found.",
	})
	// ErrBotAPIOBOChannelNotFound covers an OBO scope referencing a channel that
	// does not exist / the grantor cannot reach.
	ErrBotAPIOBOChannelNotFound = register(codes.Code{
		ID:             "err.server.bot_api.obo_channel_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Channel not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	// ErrBotAPIOBOGrantExists covers an OBO grant create that collides with an
	// already-active grant.
	ErrBotAPIOBOGrantExists = register(codes.Code{
		ID:             "err.server.bot_api.obo_grant_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "An OBO grant already exists.",
	})
	// ErrBotAPIOBOScopeExists covers an OBO scope create that collides with an
	// existing scope.
	ErrBotAPIOBOScopeExists = register(codes.Code{
		ID:             "err.server.bot_api.obo_scope_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "An OBO scope already exists.",
	})
	// ErrBotAPIMessageNotDelivered covers an edit targeting a message that has not
	// finished delivering yet (retryable).
	ErrBotAPIMessageNotDelivered = register(codes.Code{
		ID:             "err.server.bot_api.message_not_delivered",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The message has not finished delivering; please retry.",
	})

	// ---- bot auth (401, anti-enumeration) ------------------------------------

	// ErrBotAPIAuthFailed is the SINGLE anti-enumeration code for the bot-token
	// auth middleware and the legacy register endpoints: missing Authorization
	// header, invalid/unknown bot token, and unauthenticated OBO calls ALL
	// collapse to one 401 so an external caller cannot probe which factor was
	// wrong. The specific reason is logged, never returned. Rendered via
	// ResponseErrorLWithStatus to PRESERVE the real 401 (adapters branch on it).
	ErrBotAPIAuthFailed = register(codes.Code{
		ID:             "err.server.bot_api.auth_failed",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Bot authentication failed.",
	})

	// ---- upstream (502) ------------------------------------------------------

	// ErrBotAPIUpstreamFailed covers a downstream speech-service failure on the
	// voice transcribe proxy. Status-preserving (502) so the client retries.
	ErrBotAPIUpstreamFailed = register(codes.Code{
		ID:             "err.server.bot_api.upstream_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "The upstream service is unavailable.",
		Internal:       true,
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrBotAPIQueryFailed covers read-path failures (group/space/member/message
	// SELECTs, friend/membership verification, GROUP.md / voice-context reads,
	// OBO grant/scope lists). Log the underlying err before responding.
	ErrBotAPIQueryFailed = register(codes.Code{
		ID:             "err.server.bot_api.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query data.",
		Internal:       true,
	})
	// ErrBotAPIStoreFailed covers mutation-path failures (DB write/update/delete,
	// GROUP.md / voice-context writes, read-receipt inserts, message-extra
	// writes, OBO grant/scope create/revoke). Log the underlying err first.
	ErrBotAPIStoreFailed = register(codes.Code{
		ID:             "err.server.bot_api.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update data.",
		Internal:       true,
	})
	// ErrBotAPISendFailed covers WuKongIM dispatch failures (send message /
	// typing / heartbeat / messages-sync / CMD). Log the underlying err first.
	ErrBotAPISendFailed = register(codes.Code{
		ID:             "err.server.bot_api.send_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send the message.",
		Internal:       true,
	})
	// ErrBotAPIUploadFailed covers the file proxy / upload / STS-credential /
	// presigned-URL path (read, storage write, COS misconfiguration). Log first.
	ErrBotAPIUploadFailed = register(codes.Code{
		ID:             "err.server.bot_api.upload_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process the file.",
		Internal:       true,
	})
	// ErrBotAPIIMTokenFailed covers IM-token issuance failures on the bot
	// register endpoints. Log the underlying err before responding.
	ErrBotAPIIMTokenFailed = register(codes.Code{
		ID:             "err.server.bot_api.im_token_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to obtain an IM token.",
		Internal:       true,
	})
	// ErrBotAPIAuthCheckFailed covers the bot-auth middleware's own
	// infrastructure failures (DB lookup for robot / app bot errored) — distinct
	// from ErrBotAPIAuthFailed (a real credential failure). Preserves the real
	// 500 via ResponseErrorLWithStatus so adapters retry instead of treating it
	// as a permanent 401. Log the underlying err before responding.
	ErrBotAPIAuthCheckFailed = register(codes.Code{
		ID:             "err.server.bot_api.auth_check_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Bot authentication check failed.",
		Internal:       true,
	})
	// ErrBotAPIOBOInternal covers OBO grant/scope store failures and the OBO
	// permission-check infrastructure failure ("OBO 检查失败"). Log first.
	ErrBotAPIOBOInternal = register(codes.Code{
		ID:             "err.server.bot_api.obo_internal",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "OBO operation failed.",
		Internal:       true,
	})
)

// Shared codes reused by bot_api (resolved at init from the codes registry).
// The bot-auth-required defensive check on the OBO endpoints maps to the shared
// 401; generic internal asserts map to the shared 500.
var (
	ErrBotAPISharedAuthRequired = shared("err.shared.auth.required")
)
