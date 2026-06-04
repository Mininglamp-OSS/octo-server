package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.robot.* — modules/robot business error codes (api.go /
// api_manager.go / mention_pref.go). DefaultMessage holds the en-US source
// (D4); the zh-CN runtime translation lives in pkg/i18n/locales/active.zh-CN.toml.
// Internal=true codes never surface their message on the wire — callers MUST log
// the underlying err with full context (zap.Error) before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrRobotRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, "X 不能为空", invalid params). The offending
	// field is surfaced via Details when the caller can identify it.
	ErrRobotRequestInvalid = register(codes.Code{
		ID:             "err.server.robot.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrRobotContentInvalid covers an invalid message payload / content_edit
	// (empty payload, missing payload.type, payload that fails the content-type
	// contract, or content_edit that fails RichText normalization). The raw
	// payload is intentionally NOT surfaced (it can be large / sensitive); only
	// the offending field name is exposed.
	ErrRobotContentInvalid = register(codes.Code{
		ID:             "err.server.robot.content_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid message content.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrRobotContentTypeUnsupported covers an unsupported message content type.
	ErrRobotContentTypeUnsupported = register(codes.Code{
		ID:             "err.server.robot.content_type_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported message type.",
		SafeDetailKeys: []string{"type"},
	})
	// ErrRobotFileTypeUnsupported covers an unsupported / extension-less upload.
	ErrRobotFileTypeUnsupported = register(codes.Code{
		ID:             "err.server.robot.file_type_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported file type.",
	})
	// ErrRobotFileTooLarge surfaces the upload size cap so the client can render
	// a localized hint without hard-coding the limit.
	ErrRobotFileTooLarge = register(codes.Code{
		ID:             "err.server.robot.file_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The file exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"max_mb"},
	})
	// ErrRobotNoFieldsToUpdate covers an update request with no mutable fields.
	ErrRobotNoFieldsToUpdate = register(codes.Code{
		ID:             "err.server.robot.no_fields_to_update",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "No fields to update.",
	})

	// ---- permission / authorization (403) ------------------------------------

	// ErrRobotCreatorOnly covers the creator-ownership guards on the owner-facing
	// endpoints (set description / auto-approve / mention preference) — all the
	// same "only the bot creator may operate" condition.
	ErrRobotCreatorOnly = register(codes.Code{
		ID:             "err.server.robot.creator_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only the bot creator can perform this operation.",
	})
	// ErrRobotMessageEditForbidden covers the bot-message edit guard (a bot may
	// only edit messages it sent).
	ErrRobotMessageEditForbidden = register(codes.Code{
		ID:             "err.server.robot.message_edit_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You can only edit messages you sent.",
	})
	// ErrRobotChannelSendForbidden covers the channel-membership send guard.
	ErrRobotChannelSendForbidden = register(codes.Code{
		ID:             "err.server.robot.channel_send_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Sending messages to this channel is not allowed.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrRobotNotFound covers a missing / disabled / not-fully-provisioned robot
	// (no creator_uid, no app_id). No enumeration: the specific reason goes to
	// the log, the client sees one generic "not found".
	ErrRobotNotFound = register(codes.Code{
		ID:             "err.server.robot.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Robot not found.",
	})
	// ErrRobotMessageNotFound covers a missing target message on edit.
	ErrRobotMessageNotFound = register(codes.Code{
		ID:             "err.server.robot.message_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Message not found.",
	})

	// ---- robot-webhook auth (401, anti-enumeration) --------------------------

	// ErrRobotAuthFailed is the SINGLE anti-enumeration code for the robot
	// webhook auth middleware (authRobot): robot not found, app not found, and
	// app_key mismatch ALL collapse to one 401 so an external caller cannot probe
	// which factor was wrong. The specific reason is logged, never returned.
	//
	// This middleware serves external bot adapters (app_key auth), not the dmwork
	// front-end, so it is rendered via ResponseErrorLWithStatus to PRESERVE the
	// real 401 wire status rather than the D14 compatibility 400 — adapters branch
	// on HTTP 401. (Divergence from D14 flagged for maintainer sign-off.)
	ErrRobotAuthFailed = register(codes.Code{
		ID:             "err.server.robot.auth_failed",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Robot authentication failed.",
	})

	// ---- inline-query long-poll timeout (408) --------------------------------

	// ErrRobotInlineQueryTimeout covers the inline-query long-poll giving up after
	// the adapter fails to answer in time. Rendered via ResponseErrorLWithStatus
	// to preserve the 408 so the front-end's retry logic still sees a timeout.
	ErrRobotInlineQueryTimeout = register(codes.Code{
		ID:             "err.server.robot.inline_query_timeout",
		HTTPStatus:     http.StatusRequestTimeout,
		DefaultMessage: "The inline query timed out.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrRobotQueryFailed covers read-path failures (robot / menu / command /
	// message-extra SELECTs, corrupt stored command JSON). Log the underlying err
	// before responding.
	ErrRobotQueryFailed = register(codes.Code{
		ID:             "err.server.robot.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query robot data.",
		Internal:       true,
	})
	// ErrRobotStoreFailed covers mutation-path failures (DB write/update/delete,
	// transaction begin/commit/rollback, sequence generation, IM connection
	// cleanup). Log the underlying err before responding.
	ErrRobotStoreFailed = register(codes.Code{
		ID:             "err.server.robot.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update robot data.",
		Internal:       true,
	})
	// ErrRobotSendFailed covers WuKongIM dispatch failures (send message / typing
	// / stream start-end / CMD sync). Log the underlying err before responding.
	ErrRobotSendFailed = register(codes.Code{
		ID:             "err.server.robot.send_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send the message.",
		Internal:       true,
	})
	// ErrRobotUploadFailed covers the file proxy / upload / STS-credential /
	// presigned-URL path (read, storage write, COS misconfiguration). Log the
	// underlying err before responding.
	ErrRobotUploadFailed = register(codes.Code{
		ID:             "err.server.robot.upload_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process the file.",
		Internal:       true,
	})
	// ErrRobotTokenGenFailed covers bot-token (re)generation failures. Log the
	// underlying err before responding.
	ErrRobotTokenGenFailed = register(codes.Code{
		ID:             "err.server.robot.token_gen_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to generate the robot token.",
		Internal:       true,
	})
	// ErrRobotAuthCheckFailed covers the robot webhook auth middleware's own
	// infrastructure failures (DB query for robot / app errored) — distinct from
	// ErrRobotAuthFailed (a real credential failure). Preserves the real 500 via
	// ResponseErrorLWithStatus so adapters retry instead of treating it as a
	// permanent 401. Log the underlying err before responding.
	ErrRobotAuthCheckFailed = register(codes.Code{
		ID:             "err.server.robot.auth_check_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Robot authentication check failed.",
		Internal:       true,
	})
)
