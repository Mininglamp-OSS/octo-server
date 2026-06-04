package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.space.* — modules/space business error codes (api.go /
// api_manager.go / api_email_invite*.go). DefaultMessage holds the en-US source
// (D4); the zh-CN runtime translation lives in pkg/i18n/locales/active.zh-CN.toml.
// Internal=true codes never surface their message on the wire — callers MUST log
// the underlying err with full context (zap.Error) before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrSpaceRequestInvalid is the catch-all for missing/malformed request
	// input (BindJSON failure, "X 不能为空", invalid enum / mode / status / role
	// values, "at least one of ... required", negative numbers). The offending
	// field is surfaced via Details when the caller can identify it.
	ErrSpaceRequestInvalid = register(codes.Code{
		ID:             "err.server.space.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	// ErrSpaceFieldTooLong covers the name / description / logo length caps. The
	// field name and its max length are surfaced so the client can render a
	// localized hint without hard-coding the limit.
	ErrSpaceFieldTooLong = register(codes.Code{
		ID:             "err.server.space.field_too_long",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The field exceeds the maximum allowed length.",
		SafeDetailKeys: []string{"field", "max_chars"},
	})
	// ErrSpaceBatchTooLarge covers the per-request member batch cap.
	ErrSpaceBatchTooLarge = register(codes.Code{
		ID:             "err.server.space.batch_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Too many members in a single request.",
		SafeDetailKeys: []string{"max"},
	})

	// ---- permission / authorization (403) ------------------------------------

	// ErrSpacePermissionDenied covers the space-level authorization guards
	// ("no permission to ...", "only the owner may ..."). Distinct from the
	// route-level err.shared.auth.forbidden role guard.
	ErrSpacePermissionDenied = register(codes.Code{
		ID:             "err.server.space.permission_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to perform this operation.",
	})
	// ErrSpaceNotMember covers the "you are not a member of this space" guard.
	ErrSpaceNotMember = register(codes.Code{
		ID:             "err.server.space.not_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this space.",
	})
	// ErrSpaceCreationDisabled covers the admin-disabled space-creation switch.
	ErrSpaceCreationDisabled = register(codes.Code{
		ID:             "err.server.space.creation_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Space creation has been disabled by the administrator.",
	})

	// ---- not found (404) -----------------------------------------------------

	// ErrSpaceNotFound covers a missing / disbanded space.
	ErrSpaceNotFound = register(codes.Code{
		ID:             "err.server.space.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Space not found or has been disbanded.",
	})
	// ErrSpaceApplyNotFound covers a missing join-application record, or one that
	// does not belong to the current space (merged so callers cannot probe
	// cross-space application IDs).
	ErrSpaceApplyNotFound = register(codes.Code{
		ID:             "err.server.space.apply_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Join application not found.",
	})
	// ErrSpaceInviteCodeNotFound covers a missing invite code.
	ErrSpaceInviteCodeNotFound = register(codes.Code{
		ID:             "err.server.space.invite_code_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Invite code not found.",
	})
	// ErrSpaceMemberNotFound covers a missing / already-removed target member or
	// target user.
	ErrSpaceMemberNotFound = register(codes.Code{
		ID:             "err.server.space.member_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The target member does not exist or has been removed.",
	})

	// ---- invite / auth code state (400, anti-enumeration) --------------------

	// ErrSpaceInviteCodeInvalid is the single anti-enumeration code for an
	// invalid / expired / malformed invite or auth code (public preview and
	// join-approve flows). The specific reason is logged, never returned, so an
	// unauthenticated caller cannot probe which state a code is in.
	ErrSpaceInviteCodeInvalid = register(codes.Code{
		ID:             "err.server.space.invite_code_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The invite code is invalid or has expired.",
	})

	// ---- conflict / state (409) ----------------------------------------------

	// ErrSpaceInviteCodeExhausted covers an invite code that has reached its use
	// cap or is otherwise spent.
	ErrSpaceInviteCodeExhausted = register(codes.Code{
		ID:             "err.server.space.invite_code_exhausted",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The invite code has reached its usage limit.",
	})
	// ErrSpaceAlreadyMember covers re-joining a space the caller already belongs
	// to.
	ErrSpaceAlreadyMember = register(codes.Code{
		ID:             "err.server.space.already_member",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You are already a member of this space.",
	})
	// ErrSpaceApplyProcessed covers approving / rejecting an application that has
	// already been handled.
	ErrSpaceApplyProcessed = register(codes.Code{
		ID:             "err.server.space.apply_processed",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This application has already been processed.",
	})
	// ErrSpaceFull covers hitting the space member cap.
	ErrSpaceFull = register(codes.Code{
		ID:             "err.server.space.full",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The space is full.",
	})
	// ErrSpaceImmutable covers mutations rejected because the space is disbanded
	// or banned (cannot modify / add members / update status).
	ErrSpaceImmutable = register(codes.Code{
		ID:             "err.server.space.immutable",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The space has been disbanded or banned and cannot be modified.",
	})
	// ErrSpaceOwnerConstraint covers owner-specific transfer/leave/demote
	// constraints (owner must transfer ownership before leaving; cannot remove or
	// directly demote the owner).
	ErrSpaceOwnerConstraint = register(codes.Code{
		ID:             "err.server.space.owner_constraint",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Transfer ownership before performing this operation.",
	})

	// ---- email invite (member/owner email-invite accept & manage flows) ------

	// ErrSpaceEmailInviteNotFound covers a missing email invite on the admin
	// revoke path.
	ErrSpaceEmailInviteNotFound = register(codes.Code{
		ID:             "err.server.space.email_invite_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Email invite not found.",
	})
	// ErrSpaceEmailInviteInvalid covers an invite that cannot be accepted because
	// it is invalid / expired / missing required data / of an unknown type. The
	// accept endpoint is authenticated (the caller already proved identity and
	// must match the invited email), so this stays a plain 400.
	ErrSpaceEmailInviteInvalid = register(codes.Code{
		ID:             "err.server.space.email_invite_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The invite is invalid or has expired.",
	})
	// ErrSpaceEmailInviteProcessed covers accepting/revoking an invite that has
	// already been consumed, revoked, or is no longer pending.
	ErrSpaceEmailInviteProcessed = register(codes.Code{
		ID:             "err.server.space.email_invite_processed",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This invite has already been processed.",
	})
	// ErrSpaceEmailInviteEmailMismatch covers the typed-email / login-account
	// email not matching the invite target (defense-in-depth identity check).
	ErrSpaceEmailInviteEmailMismatch = register(codes.Code{
		ID:             "err.server.space.email_invite_email_mismatch",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The email does not match the invite.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrSpaceQueryFailed covers read-path failures (space / member / invite /
	// application SELECTs and counts, use-count checks, user validation). Log the
	// underlying err before responding.
	ErrSpaceQueryFailed = register(codes.Code{
		ID:             "err.server.space.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query space data.",
		Internal:       true,
	})
	// ErrSpaceStoreFailed covers mutation-path failures (create / update / join /
	// add / remove / leave / disband / role change / ownership transfer / invite
	// write, transaction begin/commit). Log the underlying err before responding.
	ErrSpaceStoreFailed = register(codes.Code{
		ID:             "err.server.space.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update space data.",
		Internal:       true,
	})
)
