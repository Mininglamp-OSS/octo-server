package workspace

import (
	"errors"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// RespondError maps the Workspace service's stable sentinels to registered,
// localized error codes. ResponseErrorL intentionally keeps the D14 transport
// status (400) while preserving each code's semantic HTTP status in the body.
// Unknown and dependency failures are logged without exposing their causes.
func RespondError(c *wkhttp.Context, err error) {
	if c == nil || err == nil {
		return
	}

	switch {
	case errors.Is(err, ErrRequestInvalid):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceRequestInvalid, nil, nil)
	case errors.Is(err, ErrSpaceRequired):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceSpaceRequired, nil, nil)
	case errors.Is(err, ErrForbidden):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceForbidden, nil, nil)
	case errors.Is(err, ErrNotFound):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceNotFound, nil, nil)
	case errors.Is(err, ErrOwnerProtected):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceOwnerProtected, nil, nil)
	case errors.Is(err, ErrRoleInvalid):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceWorkspaceRoleInvalid, nil, nil)
	case errors.Is(err, ErrCandidateIneligible):
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceCandidateIneligible, nil, nil)
	case errors.Is(err, ErrDependencyUnavailable):
		log.Error("Workspace dependency unavailable", zap.Error(err), zap.String("path", c.FullPath()))
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceDependencyUnavailable, nil, nil)
	default:
		log.Error("Workspace request failed", zap.Error(err), zap.String("path", c.FullPath()))
		httperr.ResponseErrorL(c, errcode.ErrWorkspaceInternal, nil, nil)
	}
}

func respondWorkspaceInvalid(c *wkhttp.Context, field string) {
	if field == "" {
		RespondError(c, ErrRequestInvalid)
		return
	}
	httperr.ResponseErrorL(c, errcode.ErrWorkspaceRequestInvalid, nil, i18n.Details{"field": field})
}
