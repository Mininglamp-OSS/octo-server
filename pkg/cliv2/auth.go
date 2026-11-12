package cliv2

import (
	"context"
	"errors"
	"net/http"
	"strings"

	sdk "codex.mlamp.cn/dmwork/octo-auth-sdk"
	sdkauth "codex.mlamp.cn/dmwork/octo-auth-sdk/auth"
	"codex.mlamp.cn/dmwork/octo-auth-sdk/permission"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
)

func authenticate(authClient *sdk.Client, userOnly bool) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		token, ok := bearerToken(c)
		if !ok {
			Fail(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication token is required.")
			c.Abort()
			return
		}

		access, err := authClient.Auth.Resolve(c.Request.Context(), token, nil)
		if err != nil {
			failResolve(c, err)
			c.Abort()
			return
		}
		principal, actorID, actorName, valid := trustedPrincipal(access, userOnly)
		if !valid {
			Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Authentication service unavailable.")
			c.Abort()
			return
		}

		ctx := context.WithValue(c.Request.Context(), requestAuthContextKey{}, requestAuthContext{
			principal:   principal,
			permissions: access.Permissions,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Set("uid", actorID)
		c.Set("name", actorName)
		space.SetSpaceID(c, principal.SpaceID)
		c.Next()
	}
}

func authorizeAction(action string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		auth, ok := c.Request.Context().Value(requestAuthContextKey{}).(requestAuthContext)
		if !ok {
			Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Authentication service unavailable.")
			c.Abort()
			return
		}
		if !permission.HasAction(auth.permissions, action) {
			Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		c.Next()
	}
}

func bearerToken(c *wkhttp.Context) (string, bool) {
	if c == nil || c.Request == nil {
		return "", false
	}
	values := c.Request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.Contains(parts[1], ",") {
		return "", false
	}
	return parts[1], true
}

func failResolve(c *wkhttp.Context, err error) {
	var apiErr *sdkauth.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status == http.StatusUnauthorized && apiErr.Code == "AUTH_REQUIRED" {
			Fail(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication token is invalid or expired.")
			return
		}
		if apiErr.Status == http.StatusForbidden && apiErr.Code == "FORBIDDEN" {
			Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			return
		}
	}
	Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Authentication service unavailable.")
}
