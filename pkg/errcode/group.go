package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.group.* — modules/group business error codes (api.go / api_manager.go
// / invite.go). DefaultMessage holds the en-US source (D4); the zh-CN runtime
// translation lives in pkg/i18n/locales/active.zh-CN.toml. Internal=true codes
// never surface their message on the wire — callers MUST log the underlying err
// with full context via the module logger before responding.
var (
	// ---- validation (400) ----------------------------------------------------

	// ErrGroupRequestInvalid is the catch-all for missing/malformed request
	// input (empty group_no, BindJSON failure, common.ErrData, bad action type,
	// nothing-to-update, etc.). The offending field is surfaced via Details when
	// the caller can identify it.
	ErrGroupRequestInvalid = register(codes.Code{
		ID:             "err.server.group.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrGroupMemberNotFriend = register(codes.Code{
		ID:             "err.server.group.member_not_friend",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "You can only add a friend to the group. Please add this user as a friend first.",
	})
	ErrGroupFileHelperNotAllowed = register(codes.Code{
		ID:             "err.server.group.file_helper_not_allowed",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The File Helper cannot be added to a group.",
	})
	ErrGroupCategorySpaceMismatch = register(codes.Code{
		ID:             "err.server.group.category_space_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The group category does not match the space.",
	})
	ErrGroupTargetNotBot = register(codes.Code{
		ID:             "err.server.group.target_not_bot",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The target member is not a bot.",
	})
	ErrGroupMdContentTooLarge = register(codes.Code{
		ID:             "err.server.group.group_md_content_too_large",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The GROUP.md content exceeds the maximum size.",
		SafeDetailKeys: []string{"field", "max_size"},
	})
	ErrGroupAuthCodeInvalid = register(codes.Code{
		ID:             "err.server.group.auth_code_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The authorization code is invalid or has expired.",
	})
	ErrGroupInviteExpired = register(codes.Code{
		ID:             "err.server.group.invite_expired",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The invite link has expired.",
	})

	// ---- permission / authorization (403) ------------------------------------

	ErrGroupCreatorOnly = register(codes.Code{
		ID:             "err.server.group.creator_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only the group owner can perform this action.",
	})
	ErrGroupManagerOnly = register(codes.Code{
		ID:             "err.server.group.manager_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only a group administrator can perform this action.",
	})
	ErrGroupCreatorOrManagerOnly = register(codes.Code{
		ID:             "err.server.group.creator_or_manager_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only the group owner or an administrator can perform this action.",
	})
	ErrGroupNotMember = register(codes.Code{
		ID:             "err.server.group.not_group_member",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You are not a member of this group.",
	})
	ErrGroupViewForbidden = register(codes.Code{
		ID:             "err.server.group.view_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to view this.",
	})
	ErrGroupMemberCannotRemove = register(codes.Code{
		ID:             "err.server.group.member_cannot_remove",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Regular members cannot remove group members.",
	})
	ErrGroupCannotRemoveAdmin = register(codes.Code{
		ID:             "err.server.group.cannot_remove_admin",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "An administrator cannot remove another administrator.",
	})
	ErrGroupCannotRemoveOwner = register(codes.Code{
		ID:             "err.server.group.cannot_remove_owner",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "An administrator cannot remove the group owner.",
	})
	ErrGroupExternalCannotBeAdmin = register(codes.Code{
		ID:             "err.server.group.external_cannot_be_admin",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "External members cannot be promoted to administrator.",
	})
	ErrGroupExternalCannotBeOwner = register(codes.Code{
		ID:             "err.server.group.external_cannot_be_owner",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Group ownership cannot be transferred to an external member.",
	})
	ErrGroupExternalJoinForbidden = register(codes.Code{
		ID:             "err.server.group.external_join_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This group does not allow external members to join. Please contact a group administrator.",
	})
	ErrGroupInviteModeCannotAdd = register(codes.Code{
		ID:             "err.server.group.invite_mode_cannot_add",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Invite mode is enabled; members cannot be added directly.",
	})
	ErrGroupInviteModeCannotJoin = register(codes.Code{
		ID:             "err.server.group.invite_mode_cannot_join",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Invite mode is enabled; you cannot join the group directly.",
	})
	ErrGroupCategoryForbidden = register(codes.Code{
		ID:             "err.server.group.category_forbidden",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to use this group category.",
	})
	ErrGroupBotOwnershipDenied = register(codes.Code{
		ID:             "err.server.group.bot_ownership_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have permission to invite this bot.",
	})
	ErrGroupBotNotInSpace = register(codes.Code{
		ID:             "err.server.group.bot_not_in_space",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This bot does not belong to your space.",
	})
	ErrGroupAuthCodeUserMismatch = register(codes.Code{
		ID:             "err.server.group.auth_code_user_mismatch",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The authorization code does not match the current user.",
	})
	ErrGroupQRCodeMemberOnly = register(codes.Code{
		ID:             "err.server.group.qrcode_member_only",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Only group members can generate a QR code.",
	})

	// ---- not found (404) -----------------------------------------------------

	ErrGroupNotFound = register(codes.Code{
		ID:             "err.server.group.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Group not found.",
	})
	ErrGroupMemberNotInGroup = register(codes.Code{
		ID:             "err.server.group.member_not_in_group",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The member is not in this group.",
	})
	ErrGroupCategoryNotFound = register(codes.Code{
		ID:             "err.server.group.category_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Group category not found.",
	})
	ErrGroupTransferTargetNotFound = register(codes.Code{
		ID:             "err.server.group.transfer_target_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "The target user does not exist or has been deactivated.",
	})
	ErrGroupInviteNotFound = register(codes.Code{
		ID:             "err.server.group.invite_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Invite information not found.",
	})

	// ---- conflict (409) ------------------------------------------------------

	ErrGroupCannotTargetSelf = register(codes.Code{
		ID:             "err.server.group.cannot_target_self",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You cannot perform this action on yourself.",
	})
	ErrGroupAlreadyMember = register(codes.Code{
		ID:             "err.server.group.already_member",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You are already a member of this group.",
	})
	ErrGroupInviteStatusInvalid = register(codes.Code{
		ID:             "err.server.group.invite_status_invalid",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "The invite is not in a pending state.",
	})

	// ---- too large (400) -----------------------------------------------------

	ErrGroupTooLargeToSync = register(codes.Code{
		ID:             "err.server.group.too_large_to_sync",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This group is too large to sync members.",
	})

	// ---- rate limit (429) ----------------------------------------------------

	ErrGroupDailyCreateLimit = register(codes.Code{
		ID:             "err.server.group.daily_create_limit",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "You have reached the daily group creation limit.",
	})

	// ---- internal (500, Internal=true) ---------------------------------------

	// ErrGroupQueryFailed covers read-path failures (DB SELECT/exist/count,
	// cache GET). Log the underlying err before responding.
	ErrGroupQueryFailed = register(codes.Code{
		ID:             "err.server.group.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query group data.",
		Internal:       true,
	})
	// ErrGroupStoreFailed covers mutation-path failures (DB write, transaction
	// begin/commit/rollback, event begin/commit, cache SET, serialization, file
	// upload, whitelist setup). Log the underlying err before responding.
	ErrGroupStoreFailed = register(codes.Code{
		ID:             "err.server.group.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update group data.",
		Internal:       true,
	})
	// ErrGroupNotifyFailed covers outbound IM-side failures (send command
	// message, channel update, subscribe/unsubscribe). Log the underlying err
	// before responding.
	ErrGroupNotifyFailed = register(codes.Code{
		ID:             "err.server.group.notify_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send group notification.",
		Internal:       true,
	})
)
