package messages_search

import (
	"net/http"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"go.uber.org/zap"
)

func (h *Handler) mountCLIV2Routes(source *wkhttp.WKHttp) {
	middlewares := []wkhttp.HandlerFunc{
		h.searchRateLimiter(),
		h.auditMiddleware(),
		h.backendGate(),
	}
	for _, route := range []struct {
		path    string
		handler wkhttp.HandlerFunc
	}{
		{path: "/v2/cli/messages/_search", handler: h.searchMessages},
		{path: "/v2/cli/messages/_search_all", handler: h.searchAll},
		{path: "/v2/cli/messages/_search_files", handler: h.searchFiles},
		{path: "/v2/cli/messages/_search_media", handler: h.searchMedia},
		{path: "/v2/cli/messages/_search_around", handler: h.searchAround},
		{path: "/v2/cli/messages/_search_global_groups", handler: h.searchGlobalGroups},
		{path: "/v2/cli/messages/_search_global_messages", handler: h.searchGlobalMessages},
		{path: "/v2/cli/messages/_search_global_files", handler: h.searchGlobalFiles},
	} {
		cliv2.Add(source, cliv2.Route{
			Method:      http.MethodPost,
			Path:        route.path,
			Action:      cliv2.ActionMessagesRead,
			UserOnly:    true,
			User:        h.cliv2SearchHandler(route.handler),
			Middlewares: middlewares,
		})
	}
}

func (h *Handler) cliv2SearchHandler(handler wkhttp.HandlerFunc) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		principal := cliv2.PrincipalFrom(c)
		if principal.UserID == "" || principal.SpaceID == "" {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		setPrincipal(c, userPrincipal{uid: principal.UserID, spaceID: principal.SpaceID})
		handler(c)
	}
}

func isCLIV2Search(c *wkhttp.Context) bool {
	return cliv2.PrincipalFrom(c).UserID != ""
}

func respondSearchResult(c *wkhttp.Context, result any) {
	if !isCLIV2Search(c) {
		c.Response(result)
		return
	}
	if list, ok := result.(CursorList); ok {
		c.JSON(http.StatusOK, map[string]any{
			"data":       list.Data,
			"pagination": list.Pagination,
		})
		return
	}
	cliv2.Respond(c, result)
}

func respondCLIV2SearchError(c *wkhttp.Context, status int, code, message string) bool {
	if !isCLIV2Search(c) {
		return false
	}
	cliv2.Fail(c, status, code, message)
	return true
}

func (h *Handler) checkBoundSpaceGroup(c *wkhttp.Context, groupNo, loginUID, groupSpaceID string) bool {
	principal := cliv2.PrincipalFrom(c)
	if principal.UserID == "" {
		return true
	}
	if principal.UserID != loginUID || principal.SpaceID == "" {
		respondNotFound(c, "channel")
		return false
	}
	if groupSpaceID == principal.SpaceID {
		return true
	}

	externalLookup := h.externalGroupFn
	if externalLookup == nil {
		externalLookup = group.NewDB(h.ctx).QueryExternalGroupNosForUser
	}
	externalGroups, err := externalLookup(principal.UserID)
	if err != nil {
		h.Error("messages_search: bound-Space external-group lookup failed", zap.Error(err))
		respondInternal(c)
		return false
	}
	if externalGroups[groupNo] == principal.SpaceID {
		return true
	}
	respondNotFound(c, "channel")
	return false
}
