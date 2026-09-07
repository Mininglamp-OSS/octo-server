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
	httperr.ResponseErrorL(c, errcode.ErrAITeamRequestInvalid, nil, details)
}

func respondServiceError(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, errForbidden):
		httperr.ResponseErrorL(c, errcode.ErrAITeamForbidden, nil, nil)
	case errors.Is(err, errNotFound):
		httperr.ResponseErrorL(c, errcode.ErrAITeamNotFound, nil, nil)
	case errors.Is(err, errIdempotencyConflict):
		httperr.ResponseErrorL(c, errcode.ErrAITeamIdempotencyConflict, nil, nil)
	case errors.Is(err, errIMUnavailable):
		httperr.ResponseErrorL(c, errcode.ErrAITeamIMUnavailable, nil, nil)
	default:
		httperr.ResponseErrorL(c, errcode.ErrAITeamStoreFailed, nil, nil)
	}
}
