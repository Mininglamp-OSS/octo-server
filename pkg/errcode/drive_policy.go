package errcode

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// err.server.drive_policy.* — internal drive upload-policy endpoint
// (modules/drivepolicy).
//
// The endpoint is consumed by the octo-drive backend, not by end users, and it
// is a new service-to-service surface with no legacy clients, so it renders
// with the real transport status via httperr.ResponseErrorLWithStatus. The
// design's C2 contract for this endpoint names 401 / 429 / 503 alongside 200;
// this code is the 503 arm.
var (
	// ErrDriveUploadPolicyUnavailable is returned when the process-wide
	// SystemSettings snapshot has never loaded successfully, so the effective
	// policy is genuinely unknown. Publishing the compile-time default with a
	// valid-looking fingerprint would let the drive side raise its limit on a
	// value no admin ever configured, breaking fail-closed.
	//
	// Internal=true follows the repo-wide 5xx hygiene rule (every 5xx code is
	// Internal; see TestSharedCodes_InternalFlag / TestOIDCCodesInternalFlag and
	// ErrOIDCBindServiceUnavailable, which carries the same shape). The renderer
	// then emits the shared err.shared.internal copy, so this DefaultMessage and
	// its zh-CN translation in active.zh-CN.toml are intentionally NOT reachable
	// on the wire — they are kept to satisfy Register and to document intent. The
	// octo-drive consumer tells the fail-closed 503 apart by error.code
	// (err.server.drive_policy.unavailable) and error.http_status, never by the
	// human-readable message.
	ErrDriveUploadPolicyUnavailable = register(codes.Code{
		ID:             "err.server.drive_policy.unavailable",
		HTTPStatus:     http.StatusServiceUnavailable,
		DefaultMessage: "Drive upload policy is not available yet.",
		Internal:       true,
	})
)
