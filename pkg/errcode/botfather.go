package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.botfather.* — modules/botfather business error codes. botfather
// mixes two audiences: dmwork-facing management/apply endpoints (historically
// c.ResponseError at wire 400) and external bot-adapter endpoints (bf_/uk_
// tokens) — several of which already returned a real HTTP status (401/403/404)
// that adapters branch on. Sites that already returned a non-400 real status
// are rendered via httperr.ResponseErrorLWithStatus to PRESERVE the wire status;
// the legacy c.ResponseError sites stay at wire 400 (D14).
//
// DefaultMessage holds the en-US source (D4); the zh-CN runtime translation
// lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes never
// surface their message on the wire — callers MUST log the underlying err with
// full context (zap.Error) before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrBotfatherRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, "X 不能为空", invalid id/short_id/group_no format,
	// name/username length, fileSize invalid, event_id format, unsupported file
	// type, empty GROUP.md content, invalid content_edit). The offending field is
	// surfaced via Details when identifiable.
	ErrBotfatherRequestInvalid = register(codes.Code{
		ID:             "err.server.botfather.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrBotfatherContentTooLarge covers a GROUP.md content body exceeding its
	// byte cap. The cap is surfaced via Details; the raw content never is.
	ErrBotfatherContentTooLarge = register(codes.Code{
		ID:             "err.server.botfather.content_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The content exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"field", "max_bytes"},
	})
	// ErrBotfatherFileTooLarge surfaces the upload size cap (in MB) for the
	// legacy (wire-400) upload paths.
	ErrBotfatherFileTooLarge = register(codes.Code{
		ID:             "err.server.botfather.file_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The file exceeds the maximum allowed size.",
		SafeDetailKeys: []string{"max_mb"},
	})
	// ErrBotfatherMemberNotHuman covers the bot-API rule that only human users
	// may be added to a group through this API (no bot members).
	ErrBotfatherMemberNotHuman = register(codes.Code{
		ID:             "err.server.botfather.member_not_human",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Only human members can be added through the bot API.",
	})
	// ErrBotfatherCreatorCannotBeBot rejects a group create whose creator is a
	// bot (the creator must be a human user).
	ErrBotfatherCreatorCannotBeBot = register(codes.Code{
		ID:             "err.server.botfather.creator_cannot_be_bot",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The group creator cannot be a bot.",
	})
	// ErrBotfatherThreadChannelNotAccepted rejects a thread channel id supplied
	// to a group-level GROUP.md endpoint (the caller must use the thread md route).
	ErrBotfatherThreadChannelNotAccepted = register(codes.Code{
		ID:             "err.server.botfather.thread_channel_not_accepted",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "A thread channel id is not accepted here; use the thread GROUP.md endpoint instead.",
	})
	// ErrBotfatherIDSeqMismatch covers the message-edit TOCTOU cross-check where
	// the supplied message_id does not match the resolved message_seq.
	ErrBotfatherIDSeqMismatch = register(codes.Code{
		ID:             "err.server.botfather.id_seq_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The message id does not match the message sequence.",
	})
	// ErrBotfatherCannotApplyOwnBot covers a user applying to use a bot they own.
	ErrBotfatherCannotApplyOwnBot = register(codes.Code{
		ID:             "err.server.botfather.cannot_apply_own_bot",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "No need to apply for a bot you own.",
	})

	// ---- permission / authorization (403) ------------------------------------

	// ErrBotfatherNotOwner covers the apply approve/reject guard where the caller
	// is not the bot's owner. Legacy wire 400 (D14).
	ErrBotfatherNotOwner = register(codes.Code{
		ID:             "err.server.botfather.not_owner",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not the owner of this bot.",
	})
	// ErrBotfatherBotNotGroupMember covers the bot-not-a-group-member guard. The
	// AbortWithStatusJSON 403 sites preserve the real 403 via WithStatus; the
	// legacy c.ResponseError sites stay wire 400.
	ErrBotfatherBotNotGroupMember = register(codes.Code{
		ID:             "err.server.botfather.bot_not_group_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not a member of this group.",
	})
	// ErrBotfatherBotNotGroupAdmin covers the bot-not-a-bot_admin guard.
	ErrBotfatherBotNotGroupAdmin = register(codes.Code{
		ID:             "err.server.botfather.bot_not_group_admin",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not an admin of this group.",
	})
	// ErrBotfatherBotNotSpaceMember covers the bot-not-a-space-member guard on
	// the space members endpoint (AbortWithStatusJSON 403 → preserve 403).
	ErrBotfatherBotNotSpaceMember = register(codes.Code{
		ID:             "err.server.botfather.bot_not_space_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot is not a member of this space.",
	})
	// ErrBotfatherBotNotInSpace covers the User-API Space-isolation guard where
	// the bot does not belong to the API key's Space (AbortWithStatusJSON 403 →
	// preserve 403).
	ErrBotfatherBotNotInSpace = register(codes.Code{
		ID:             "err.server.botfather.bot_not_in_space",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The bot does not belong to the current space.",
	})
	// ErrBotfatherMessageEditForbidden covers the bot-message edit guard (a bot
	// may only edit messages it sent). Legacy wire 400 (D14).
	ErrBotfatherMessageEditForbidden = register(codes.Code{
		ID:             "err.server.botfather.message_edit_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You can only edit messages you sent.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrBotfatherRobotNotFound covers a target robot that does not exist or has
	// been deleted on the apply path. Legacy wire 400 (D14).
	ErrBotfatherRobotNotFound = register(codes.Code{
		ID:             "err.server.botfather.robot_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The bot does not exist or has been deleted.",
	})
	// ErrBotfatherApplyNotFound covers a missing apply record. Legacy wire 400.
	ErrBotfatherApplyNotFound = register(codes.Code{
		ID:             "err.server.botfather.apply_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The application record does not exist.",
	})
	// ErrBotfatherMessageNotFound covers a missing target message on edit.
	// Legacy wire 400 (D14).
	ErrBotfatherMessageNotFound = register(codes.Code{
		ID:             "err.server.botfather.message_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Message not found.",
	})
	// ErrBotfatherUserNotFound covers a missing target user (getUserInfo raw
	// c.JSON 404 → preserve 404). External adapters branch on HTTP 404.
	ErrBotfatherUserNotFound = register(codes.Code{
		ID:             "err.server.botfather.user_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "User not found.",
	})
	// ErrBotfatherBotNotFound covers a missing / not-owned User-API bot
	// (AbortWithStatusJSON 404 → preserve 404). Existence-leak-safe: ownership
	// mismatch also maps here so a caller cannot probe other users' bot ids.
	ErrBotfatherBotNotFound = register(codes.Code{
		ID:             "err.server.botfather.bot_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The bot does not exist or you do not have permission.",
	})

	// ---- conflict (409) ------------------------------------------------------

	// ErrBotfatherApplyProcessed covers an approve/reject on an already-processed
	// apply. Legacy wire 400 (D14).
	ErrBotfatherApplyProcessed = register(codes.Code{
		ID:             "err.server.botfather.apply_processed",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The application has already been processed.",
	})
	// ErrBotfatherApplyExists covers a duplicate pending apply. Legacy wire 400.
	ErrBotfatherApplyExists = register(codes.Code{
		ID:             "err.server.botfather.apply_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You have already submitted an application; please wait for the owner to review it.",
	})
	// ErrBotfatherAlreadyFriends covers an apply where the user and bot are
	// already friends. Legacy wire 400 (D14).
	ErrBotfatherAlreadyFriends = register(codes.Code{
		ID:             "err.server.botfather.already_friends",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You are already friends.",
	})
	// ErrBotfatherUsernameTaken covers a username collision on bot create
	// (ResponseErrorWithStatus 409 → preserve 409). The offending username is
	// surfaced via Details so the client can render a localized hint.
	ErrBotfatherUsernameTaken = register(codes.Code{
		ID:             "err.server.botfather.username_taken",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The username is already taken.",
		SafeDetailKeys: []string{"username"},
	})

	// ---- bot auth (401, anti-enumeration) ------------------------------------

	// ErrBotfatherAuthFailed is the SINGLE anti-enumeration code for the bot/User
	// API Key auth middleware and the legacy register endpoint: missing
	// Authorization header, invalid/unknown bot token, invalid API Key, and
	// authentication-lookup denials ALL collapse to one 401 so an external caller
	// cannot probe which factor was wrong. The specific reason is logged, never
	// returned. The middleware sites preserve the real 401 via
	// ResponseErrorLWithStatus (adapters branch on it) + c.Abort; the legacy
	// register endpoint stays wire 400 (D14).
	ErrBotfatherAuthFailed = register(codes.Code{
		ID:             "err.server.botfather.auth_failed",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Bot authentication failed.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrBotfatherQueryFailed covers read-path failures (robot/group/member/
	// message/apply SELECTs, friend/membership verification, GROUP.md reads, apply
	// list/count). Log the underlying err before responding.
	ErrBotfatherQueryFailed = register(codes.Code{
		ID:             "err.server.botfather.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query data.",
		Internal:       true,
	})
	// ErrBotfatherStoreFailed covers mutation-path failures (bot/apply/friend/
	// command create-update-delete, GROUP.md writes, message_extra writes, group/
	// thread service mutations, event ack). Log the underlying err first.
	ErrBotfatherStoreFailed = register(codes.Code{
		ID:             "err.server.botfather.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update data.",
		Internal:       true,
	})
	// ErrBotfatherSendFailed covers WuKongIM dispatch failures (send message /
	// typing / messages-sync / heartbeat). Log the underlying err first.
	ErrBotfatherSendFailed = register(codes.Code{
		ID:             "err.server.botfather.send_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send the message.",
		Internal:       true,
	})
	// ErrBotfatherUploadFailed covers the file proxy / upload / STS-credential /
	// presigned-URL path (read, storage write, COS misconfiguration, download URL
	// generation). Log the underlying err first.
	ErrBotfatherUploadFailed = register(codes.Code{
		ID:             "err.server.botfather.upload_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process the file.",
		Internal:       true,
	})
	// ErrBotfatherIMTokenFailed covers IM-token issuance failures on the legacy
	// register endpoint. Log the underlying err before responding.
	ErrBotfatherIMTokenFailed = register(codes.Code{
		ID:             "err.server.botfather.im_token_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to obtain an IM token.",
		Internal:       true,
	})
)
