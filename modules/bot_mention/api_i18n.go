package bot_mention

import (
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func respondBotMentionUnauthorized(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
}

func respondBotMentionInvalid(c *wkhttp.Context, field string) {
	var details i18n.Details
	if field != "" {
		details = i18n.Details{"field": field}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, details)
}

func respondBotMentionNotFound(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
}

func respondBotMentionInProgress(c *wkhttp.Context) {
	c.Header("Retry-After", strconv.Itoa(int(claimPendingTTL/time.Second)))
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotMentionInProgress, nil, nil)
}

func respondBotMentionConflict(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotMentionIdempotencyConflict, nil, nil)
}

func respondBotMentionStoreFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrBotMentionStoreFailed, nil, nil)
}
