package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// User module error codes. Migrated from modules/user/api.go legacy
// c.ResponseError sites in Phase 2.1 (~244 sites collapsed to ~42 codes).
//
// SafeDetailKeys are intentionally minimal: `field` for parameter / state
// guards so the client can highlight the offending input, plus a handful of
// per-code keys where the message inherently needs more context (lock-screen
// minute bounds, WeChat response missing field).
//
// Internal=true is set on 5xx failures so the renderer suppresses the
// DefaultMessage / params / details on the wire — operators still get the
// underlying error in zap logs.
var (
	// Parameter / format guards.

	ErrUserRequestInvalid = register(codes.Code{
		ID:             "err.server.user.request_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid request.",
		SafeDetailKeys: []string{"field"},
	})
	ErrUserLockMinuteOutOfRange = register(codes.Code{
		ID:             "err.server.user.lock_minute_out_of_range",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Lock screen delay must be between 0 and 60 minutes.",
		SafeDetailKeys: []string{"field", "min", "max"},
	})
	ErrUserShortNoFormatInvalid = register(codes.Code{
		ID:             "err.server.user.short_no_format_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Short ID must start with a letter and contain 6-20 letters, digits, underscores or hyphens.",
	})
	ErrUserLanguageUnsupported = register(codes.Code{
		ID:             "err.server.user.language_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Unsupported language.",
	})
	ErrUserTokenRequired = register(codes.Code{
		ID:             "err.server.user.token_required",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Token is required.",
		SafeDetailKeys: []string{"field"},
	})

	// Auth credentials & session.

	ErrUserInvalidCredentials = register(codes.Code{
		ID:             "err.server.user.invalid_credentials",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Invalid username or password.",
	})
	ErrUserCodeInvalid = register(codes.Code{
		ID:             "err.server.user.code_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid verification code.",
	})
	ErrUserAccountBanned = register(codes.Code{
		ID:             "err.server.user.account_banned",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This account has been banned.",
	})
	ErrUserLoginDeviceExpired = register(codes.Code{
		ID:             "err.server.user.login_device_expired",
		HTTPStatus:     http.StatusUnauthorized,
		DefaultMessage: "Login device session expired, please sign in again.",
	})
	// ErrUserLoginLocked maps the anti-brute-force lockout returned by
	// LoginGuard.Check (ErrLoginLocked). Not Internal=true: the wire message
	// is the user-actionable explanation. 429 is the standard HTTP status
	// for rate-limited / lockout states even though the count window is
	// per-account rather than per-IP.
	ErrUserLoginLocked = register(codes.Code{
		ID:             "err.server.user.login_locked",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "Too many failed login attempts, account temporarily locked. Please try again later.",
	})

	// Existence.

	ErrUserNotFound = register(codes.Code{
		ID:             "err.server.user.not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "User not found.",
	})
	ErrUserCurrentNotFound = register(codes.Code{
		ID:             "err.server.user.current_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Current user not found.",
	})
	ErrUserDeviceNotFound = register(codes.Code{
		ID:             "err.server.user.device_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Device not found.",
	})
	ErrUserAlreadyExists = register(codes.Code{
		ID:             "err.server.user.already_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "User already exists.",
	})

	// Registration / login policy.

	ErrUserRegistrationClosed = register(codes.Code{
		ID:             "err.server.user.registration_closed",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Registration is currently closed.",
	})
	ErrUserLocalLoginDisabled = register(codes.Code{
		ID:             "err.server.user.local_login_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Local login is disabled.",
	})
	ErrUserPhoneRegionUnsupported = register(codes.Code{
		ID:             "err.server.user.phone_region_unsupported",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Only mainland China phone numbers are supported.",
	})
	ErrUserInviteCodeNotFound = register(codes.Code{
		ID:             "err.server.user.invite_code_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Invite code does not exist.",
	})

	// Account destroy lifecycle.

	ErrUserAccountDestroyed = register(codes.Code{
		ID:             "err.server.user.account_destroyed",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This account has been deactivated.",
	})
	ErrUserAccountDestroying = register(codes.Code{
		ID:             "err.server.user.account_destroying",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Account is in the deactivation cooldown period, please use a newer client to revoke or check status.",
	})

	// Profile update guards.

	ErrUserUpdateNotAllowed = register(codes.Code{
		ID:             "err.server.user.update_not_allowed",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This field cannot be updated.",
		SafeDetailKeys: []string{"field"},
	})
	ErrUserShortNoAlreadyChanged = register(codes.Code{
		ID:             "err.server.user.short_no_already_changed",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Short ID can only be changed once.",
	})
	ErrUserDemoLockUnsupported = register(codes.Code{
		ID:             "err.server.user.demo_lock_unsupported",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Demo accounts cannot enable device lock.",
	})

	// QR-code-based login.

	ErrUserAuthCodeNotFound = register(codes.Code{
		ID:             "err.server.user.auth_code_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Authorization code is invalid or has expired.",
	})
	ErrUserAuthCodeWrongType = register(codes.Code{
		ID:             "err.server.user.auth_code_wrong_type",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Authorization code is not a login code.",
	})
	ErrUserAuthInfoInvalid = register(codes.Code{
		ID:             "err.server.user.auth_info_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Authorization payload is invalid.",
		SafeDetailKeys: []string{"missing_field"},
	})
	ErrUserAuthScannerMismatch = register(codes.Code{
		ID:             "err.server.user.auth_scanner_mismatch",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Scanner and authorizer are not the same user.",
	})
	ErrUserQRVerCodeMissing = register(codes.Code{
		ID:             "err.server.user.qr_ver_code_missing",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "User has no QR verification code.",
	})

	// WeChat third-party integration (502 Bad Gateway, Internal=true).

	ErrUserWeChatExchangeFailed = register(codes.Code{
		ID:             "err.server.user.wechat_exchange_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Failed to exchange WeChat access token.",
		Internal:       true,
	})
	ErrUserWeChatProfileFailed = register(codes.Code{
		ID:             "err.server.user.wechat_profile_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Failed to fetch WeChat user profile.",
		Internal:       true,
	})
	ErrUserWeChatResponseInvalid = register(codes.Code{
		ID:             "err.server.user.wechat_response_invalid",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "WeChat response is malformed.",
		Internal:       true,
	})

	// User-visible password / lock-screen operations (distinguished for ops monitoring).

	ErrUserChatPwdUpdateFailed = register(codes.Code{
		ID:             "err.server.user.chat_pwd_update_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update chat password.",
		Internal:       true,
	})
	ErrUserLoginPwdUpdateFailed = register(codes.Code{
		ID:             "err.server.user.login_pwd_update_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update login password.",
		Internal:       true,
	})
	ErrUserLockScreenPwdUpdateFailed = register(codes.Code{
		ID:             "err.server.user.lock_screen_pwd_update_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update lock-screen password.",
		Internal:       true,
	})
	ErrUserPasswordProcessFailed = register(codes.Code{
		ID:             "err.server.user.password_process_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process password.",
		Internal:       true,
	})

	// Internal failures (500, Internal=true).

	ErrUserQueryFailed = register(codes.Code{
		ID:             "err.server.user.query_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to query user data.",
		Internal:       true,
	})
	ErrUserStoreFailed = register(codes.Code{
		ID:             "err.server.user.store_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to persist user data.",
		Internal:       true,
	})
	ErrUserIMCallFailed = register(codes.Code{
		ID:             "err.server.user.im_call_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to call IM service.",
		Internal:       true,
	})
	ErrUserDecodeFailed = register(codes.Code{
		ID:             "err.server.user.decode_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to decode internal payload.",
		Internal:       true,
	})
	ErrUserFileOperationFailed = register(codes.Code{
		ID:             "err.server.user.file_operation_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to process file.",
		Internal:       true,
	})
	ErrUserSMSSendFailed = register(codes.Code{
		ID:             "err.server.user.sms_send_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send SMS.",
		Internal:       true,
	})
	ErrUserDestroyFailed = register(codes.Code{
		ID:             "err.server.user.destroy_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to deactivate account.",
		Internal:       true,
	})
	ErrUserRegisterFailed = register(codes.Code{
		ID:             "err.server.user.register_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to register user.",
		Internal:       true,
	})
	ErrUserLanguageSetFailed = register(codes.Code{
		ID:             "err.server.user.language_set_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to set language preference.",
		Internal:       true,
	})

	// Management-console (modules/user/api_manager.go) codes. Migrated from
	// the legacy c.ResponseError(errors.New("中文")) sites in Phase 2.1. Shared
	// auth/param/internal cases reuse err.shared.* (forbidden / param.invalid)
	// and the generic ErrUser* codes above; the codes below cover the
	// manager-specific guards that had no existing equivalent.

	// ErrUserManagerPermissionRequired fires at /v1/manager/login after the
	// password check succeeds but the account carries no admin/superAdmin role.
	// Distinct from err.shared.auth.forbidden (a route-level role guard) so the
	// login page can show a login-specific hint.
	ErrUserManagerPermissionRequired = register(codes.Code{
		ID:             "err.server.user.manager_permission_required",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This account does not have management permission.",
	})
	ErrUserPasswordTooShort = register(codes.Code{
		ID:             "err.server.user.password_too_short",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Password must be at least 6 characters.",
	})
	ErrUserPasswordMismatch = register(codes.Code{
		ID:             "err.server.user.password_mismatch",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The two passwords do not match.",
	})
	ErrUserOldPasswordIncorrect = register(codes.Code{
		ID:             "err.server.user.old_password_incorrect",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The current password is incorrect.",
	})
	ErrUserNewPasswordSameAsOld = register(codes.Code{
		ID:             "err.server.user.new_password_same_as_old",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The new password must be different from the current one.",
	})
	// ErrUserNotAdminAccount guards the delete-admin endpoint: the target user
	// exists but is not an administrator account, so it cannot be deleted here.
	ErrUserNotAdminAccount = register(codes.Code{
		ID:             "err.server.user.not_admin_account",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This user is not an administrator account and cannot be deleted.",
	})
	ErrUserCannotDeleteSuperAdmin = register(codes.Code{
		ID:             "err.server.user.cannot_delete_super_admin",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "The super administrator account cannot be deleted.",
	})
	// ErrUserListFilterConflict reports mutually-exclusive list filters
	// (bot_only + exclude_bot, system_only + exclude_system). The conflicting
	// filter names are surfaced so a frontend dev can spot the bad query.
	ErrUserListFilterConflict = register(codes.Code{
		ID:             "err.server.user.list_filter_conflict",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Conflicting list filters.",
		SafeDetailKeys: []string{"filter", "conflicts_with"},
	})

	// Internal failures (500, Internal=true).

	// ErrUserTokenCacheFailed covers the session-token cache read/write/delete
	// failures in the manager login / password-change / delete-admin flows.
	ErrUserTokenCacheFailed = register(codes.Code{
		ID:             "err.server.user.token_cache_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to update the session token cache.",
		Internal:       true,
	})
	ErrUserShortNoGenFailed = register(codes.Code{
		ID:             "err.server.user.short_no_gen_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to generate a short ID.",
		Internal:       true,
	})

	// Friend / contact (modules/user/api_friend.go) codes. Most legacy sites
	// there are internal DB / IM / cache failures that collapse to the generic
	// ErrUserQueryFailed / ErrUserStoreFailed / ErrUserIMCallFailed /
	// ErrUserTokenCacheFailed / ErrUserDecodeFailed codes above; the codes
	// below cover the friend-specific user-facing business guards.

	ErrUserCannotAddSelf = register(codes.Code{
		ID:             "err.server.user.cannot_add_self",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "You cannot add yourself as a friend.",
	})
	ErrUserAlreadyFriend = register(codes.Code{
		ID:             "err.server.user.already_friend",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "You are already friends.",
	})
	ErrUserBotNotInSpace = register(codes.Code{
		ID:             "err.server.user.bot_not_in_space",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This bot is not in the current space and cannot be added.",
	})
	ErrUserFriendApplyNotFound = register(codes.Code{
		ID:             "err.server.user.friend_apply_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Friend request not found.",
	})
	ErrUserFriendNotFound = register(codes.Code{
		ID:             "err.server.user.friend_not_found",
		HTTPStatus:     http.StatusNotFound,
		DefaultMessage: "Friend not found.",
	})
	// ErrUserFriendApplyInvalid covers an expired / malformed friend-request
	// token or payload (the apply token decoded but its referenced records are
	// gone or inconsistent).
	ErrUserFriendApplyInvalid = register(codes.Code{
		ID:             "err.server.user.friend_apply_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Friend request is invalid or has expired.",
	})

	// Account deactivation (modules/user/api_destroy.go) codes. Reuse
	// account_destroying / account_destroyed / destroy_failed above; the codes
	// below cover the destroy-specific guards.

	ErrUserPasswordNotSet = register(codes.Code{
		ID:             "err.server.user.password_not_set",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This account has no password set; identity cannot be verified.",
	})
	ErrUserPasswordIncorrect = register(codes.Code{
		ID:             "err.server.user.password_incorrect",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Incorrect password.",
	})
	// ErrUserAccountStateChanged maps the optimistic-lock conflict
	// (ErrDestroyStateConflict) when a concurrent request changed the
	// deactivation state out from under this one.
	ErrUserAccountStateChanged = register(codes.Code{
		ID:             "err.server.user.account_state_changed",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "Account status has changed, please refresh and try again.",
	})
	ErrUserNotDestroying = register(codes.Code{
		ID:             "err.server.user.not_destroying",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Account is not pending deactivation.",
	})

	// Pinned channels (modules/user/api_pinned.go) codes.

	ErrUserPinnedAlreadyExists = register(codes.Code{
		ID:             "err.server.user.pinned_already_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "This channel is already pinned.",
	})
	ErrUserPinnedLimitExceeded = register(codes.Code{
		ID:             "err.server.user.pinned_limit_exceeded",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "The pinned channel limit has been reached.",
		SafeDetailKeys: []string{"max"},
	})
	// ErrUserChannelAccessDenied collapses the validateChannelAccess guard
	// rejections (not a friend / not a group member / bot not added). Internal
	// check failures inside that helper are logged server-side and also surface
	// here; splitting them into a 5xx is a follow-up.
	ErrUserChannelAccessDenied = register(codes.Code{
		ID:             "err.server.user.channel_access_denied",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "You do not have access to this channel.",
	})
	ErrUserPinnedSortInvalid = register(codes.Code{
		ID:             "err.server.user.pinned_sort_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid pinned sort request.",
	})

	// Third-party OAuth login (modules/user/api_gitee.go, api_github.go) codes.

	ErrUserOAuthStateExpired = register(codes.Code{
		ID:             "err.server.user.oauth_state_expired",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Login session has expired, please try again.",
	})
	ErrUserOAuthExchangeFailed = register(codes.Code{
		ID:             "err.server.user.oauth_exchange_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Failed to exchange the OAuth access token.",
		Internal:       true,
	})
	ErrUserOAuthProfileFailed = register(codes.Code{
		ID:             "err.server.user.oauth_profile_failed",
		HTTPStatus:     http.StatusBadGateway,
		DefaultMessage: "Failed to fetch the OAuth user profile.",
		Internal:       true,
	})

	// Email login / registration (modules/user/api_emaillogin.go) codes. Reuse
	// registration_closed / local_login_disabled / invalid_credentials /
	// already_exists / not_found / password_too_short / code_invalid /
	// register_failed / login_pwd_update_failed / password_process_failed above.

	ErrUserEmailInvalid = register(codes.Code{
		ID:             "err.server.user.email_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid email address.",
	})
	ErrUserEmailRegisterDisabled = register(codes.Code{
		ID:             "err.server.user.email_register_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Email registration is not available.",
	})
	ErrUserEmailLoginDisabled = register(codes.Code{
		ID:             "err.server.user.email_login_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Email login is not available.",
	})
	// ErrUserAccountUnavailable covers the code-login branch that surfaces a
	// deactivated-or-banned account (the password branch unifies onto
	// invalid_credentials to avoid enumeration).
	ErrUserAccountUnavailable = register(codes.Code{
		ID:             "err.server.user.account_unavailable",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "This account has been deactivated or disabled.",
	})
	ErrUserEmailSendFailed = register(codes.Code{
		ID:             "err.server.user.email_send_failed",
		HTTPStatus:     http.StatusInternalServerError,
		DefaultMessage: "Failed to send the email verification code.",
		Internal:       true,
	})
	// ErrUserEmailRateLimited is the client-actionable resend cooldown (the
	// 1-minute throttle in EmailService.SendVerifyCode). 429, not Internal, so
	// the actionable "try again shortly" message reaches the client.
	ErrUserEmailRateLimited = register(codes.Code{
		ID:             "err.server.user.email_rate_limited",
		HTTPStatus:     http.StatusTooManyRequests,
		DefaultMessage: "Verification codes are being sent too frequently, please try again in a minute.",
	})

	// Username / Web3 signature login (modules/user/api_usernamelogin.go) codes.
	// Reuse registration_closed / local_login_disabled / invalid_credentials /
	// already_exists / not_found / current_not_found / login_locked /
	// password_too_short / old_password_incorrect / new_password_same_as_old /
	// password_process_failed / login_pwd_update_failed / register_failed /
	// query_failed / store_failed / token_cache_failed above.

	ErrUserUsernameRegisterDisabled = register(codes.Code{
		ID:             "err.server.user.username_register_disabled",
		HTTPStatus:     http.StatusForbidden,
		DefaultMessage: "Username registration is not available.",
	})
	ErrUserUsernameFormatInvalid = register(codes.Code{
		ID:             "err.server.user.username_format_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Username must be 8-22 characters.",
	})
	ErrUserPublicKeyNotFound = register(codes.Code{
		ID:             "err.server.user.public_key_not_found",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "This user has not uploaded a public key.",
	})
	ErrUserPublicKeyAlreadyExists = register(codes.Code{
		ID:             "err.server.user.public_key_already_exists",
		HTTPStatus:     http.StatusConflict,
		DefaultMessage: "A public key has already been uploaded for this user.",
	})
	// ErrUserSignatureNotFound covers a missing / expired Web3 verify-text
	// challenge (the cached nonce the client must sign is gone or mismatched).
	ErrUserSignatureNotFound = register(codes.Code{
		ID:             "err.server.user.signature_not_found",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Signature challenge does not exist or has expired.",
	})
	ErrUserSignatureInvalid = register(codes.Code{
		ID:             "err.server.user.signature_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Signature verification failed.",
	})
	ErrUserVerifyTypeInvalid = register(codes.Code{
		ID:             "err.server.user.verify_type_invalid",
		HTTPStatus:     http.StatusBadRequest,
		DefaultMessage: "Invalid verification type.",
	})
)
