package ai_team

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

func respondInvalid(c *wkhttp.Context, field string) {
	details := i18n.Details{}
	if field != "" {
		details["field"] = field
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamRequestInvalid, nil, details)
}

func respondServiceError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, errInvalid):
		respondInvalid(c, "body")
	case errors.Is(err, errForbidden):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamForbidden, nil, nil)
	case errors.Is(err, errTeamCreateLimit):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrGroupDailyCreateLimit, nil, nil)
	case errors.Is(err, errNotFound):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamNotFound, nil, nil)
	case errors.Is(err, errIdempotencyConflict):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamIdempotencyConflict, nil, nil)
	case errors.Is(err, errIMUnavailable):
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamIMUnavailable, nil, nil)
	default:
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamStoreFailed, nil, nil)
	}
}
