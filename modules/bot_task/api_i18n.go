package bot_task

import (
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func respondUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
}
func respondInvalid(c *wkhttp.Context, field string) {
	var details i18n.Details
	if field != "" {
		details = i18n.Details{"field": field}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, details)
}
func respondNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
}
func respondForbidden(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotTaskForbidden, nil, nil)
}
func respondInProgress(c *wkhttp.Context) {
	c.Header("Retry-After", strconv.Itoa(int(claimRetryAfter/time.Second)))
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotTaskInProgress, nil, nil)
}
func respondConflict(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotTaskIdempotencyConflict, nil, nil)
}
func respondStoreFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotTaskStoreFailed, nil, nil)
}
