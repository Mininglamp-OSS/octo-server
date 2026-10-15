package drivepolicy

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
)

// respondUnauthorized emits the shared 401 anti-enumeration envelope. Matches
// what modules/internal_resolve and modules/bot_mention return on
// X-Internal-Token failures, so external callers see a uniform "unauthorized"
// surface across internal APIs and cannot tell "unset" from "wrong".
func respondUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
}

// respondUnavailable emits the fail-closed 503 used when the process-wide
// SystemSettings snapshot has never loaded successfully, so the effective
// policy is genuinely unknown. Answering 200 with a compile-time default and a
// valid-looking fingerprint would let the drive side raise its limit on a value
// no admin ever configured, which is exactly the failure mode design C2 makes
// this channel fail-closed against. A later successful auto-reload self-heals,
// so the drive side should treat 503 as "retry later / keep current limits",
// not as "policy is empty". ResponseErrorLWithStatus renders the real transport
// status (503) because this is a new service-to-service endpoint with no legacy
// clients depending on the fixed-400 compatibility path.
func respondUnavailable(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrDriveUploadPolicyUnavailable, nil, nil)
}
