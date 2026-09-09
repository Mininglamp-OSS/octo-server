package workspace

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/Mininglamp-OSS/octo-server/pkg/ratelimit"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const (
	workspaceInternalRateLimitTag   = "workspace_internal"
	workspaceInternalIPRPSEnv       = "DM_WORKSPACE_INTERNAL_IP_RPS"
	workspaceInternalIPBurstEnv     = "DM_WORKSPACE_INTERNAL_IP_BURST"
	workspaceInternalIPRPSDefault   = 2.0
	workspaceInternalIPBurstDefault = 60
)

func (a *API) workspaceInternalIPRateLimit(r *wkhttp.WKHttp) wkhttp.HandlerFunc {
	rlRedis := octoredis.NewInstrumentedClient(a.ctx.GetConfig(), func(options *rd.Options) {
		options.MaxRetries = 1
		options.PoolSize = 10
	})
	rps := ratelimit.SanitizeRPS(
		wkhttp.ParseRPSFromEnv(workspaceInternalIPRPSEnv, workspaceInternalIPRPSDefault),
		workspaceInternalIPRPSDefault,
	)
	burst := ratelimit.SanitizeBurst(
		wkhttp.ParseBurstFromEnv(workspaceInternalIPBurstEnv, workspaceInternalIPBurstDefault),
		workspaceInternalIPBurstDefault,
	)
	return r.StrictIPRateLimitMiddleware(context.Background(), rlRedis, workspaceInternalRateLimitTag, rps, burst)
}

// Both credentials authorize the same read-only internal Workspace surface;
// they identify peer deployments, not distinct capabilities.
func (a *API) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token := c.GetHeader(workspaceInternalTokenHeader)
		loopOK := a.loopToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.loopToken)) == 1
		driveOK := a.driveToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.driveToken)) == 1
		if !loopOK && !driveOK {
			httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedTokenInvalid, nil, nil)
			c.Abort()
			return
		}
		c.Next()
	}
}

func (a *API) internalListWorkspaces(c *wkhttp.Context) {
	result, err := a.service.InternalList(strings.TrimSpace(c.Query("space_id")), ParsePage(c))
	if err != nil {
		a.respondInternalError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) internalGetWorkspace(c *wkhttp.Context) {
	workspaceID := strings.TrimSpace(c.Param("workspace_id"))
	if workspaceID == "" {
		respondWorkspaceInternalParam(c, "workspace_id")
		return
	}
	result, err := a.service.InternalGet(workspaceID)
	if err != nil {
		a.respondInternalError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) internalListMembers(c *wkhttp.Context) {
	workspaceID := strings.TrimSpace(c.Param("workspace_id"))
	if workspaceID == "" {
		respondWorkspaceInternalParam(c, "workspace_id")
		return
	}
	filter, err := parseMemberFilter(c)
	if err != nil {
		a.respondInternalError(c, err)
		return
	}
	result, err := a.service.InternalMembers(workspaceID, filter, ParsePage(c))
	if err != nil {
		a.respondInternalError(c, err)
		return
	}
	c.Response(result)
}

func (a *API) respondInternalError(c *wkhttp.Context, err error) {
	if errors.Is(err, ErrRequestInvalid) || errors.Is(err, ErrRoleInvalid) {
		respondWorkspaceInternalParam(c, "")
		return
	}
	if errors.Is(err, ErrNotFound) {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}
	a.Error("workspace internal request failed", zap.Error(err), zap.String("path", c.FullPath()))
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedInternal, nil, nil)
}

func respondWorkspaceInternalParam(c *wkhttp.Context, field string) {
	var details i18n.Details
	if field != "" {
		details = i18n.Details{"field": field}
	}
	httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedParamInvalid, nil, details)
}
