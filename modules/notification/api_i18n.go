package notification

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
)

func respondInvalidPauseTime(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrNotificationPauseInvalidTime, nil, nil)
}

func respondPauseUpdateFailed(c *wkhttp.Context) {
	httperr.ResponseErrorLWithStatus(c, errcode.ErrNotificationPauseUpdateFailed, nil, nil)
}
