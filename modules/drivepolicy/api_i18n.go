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
