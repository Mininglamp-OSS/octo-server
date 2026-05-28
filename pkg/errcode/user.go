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
)
