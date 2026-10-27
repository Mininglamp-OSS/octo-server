package workspacecapability

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type API struct {
	ctx   *config.Context
	store *Store
	log.Log
}

func New(ctx *config.Context) *API {
	var store *Store
	if db := ctx.DB(); db != nil && db.DB != nil {
		store = NewStore(db.DB)
	}
	return &API{ctx: ctx, store: store, Log: log.NewTLog("WorkspaceCapability")}
}

func (a *API) Route(r *wkhttp.WKHttp) {
	g := r.Group("/v1/workspaces/:workspace_id",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
		spacepkg.SpaceMiddleware(a.ctx),
	)
	g.GET("/capabilities", a.list)
	g.POST("/capabilities/import", a.importResources)
	g.GET("/capabilities/:capability_id", a.get)
	g.DELETE("/capabilities/:capability_id", a.deleteResource)
	g.POST("/capabilities/:capability_id/authorization", a.setAuthorization)
	g.GET("/experts/:expert_id/bindings", a.listBindings)
	g.POST("/experts/:expert_id/bindings", a.bind)
	g.DELETE("/experts/:expert_id/bindings/:capability_id", a.unbind)
}

func (a *API) list(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	kind := Kind(strings.TrimSpace(c.Query("kind")))
	if kind != "" && kind != KindSkill && kind != KindConnector {
		badRequest(c, "invalid_kind", "kind must be skill or connector")
		return
	}
	items, err := a.store.List(c.Request.Context(), scope, kind, c.Query("q"))
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items}})
}

func (a *API) importResources(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	var body struct {
		Items []ImportItem `json:"items"`
	}
	if err := decodeJSON(c, &body, 32<<20); err != nil {
		badRequest(c, "invalid_request", err.Error())
		return
	}
	if len(body.Items) == 0 {
		badRequest(c, "invalid_request", "items are required")
		return
	}
	if len(body.Items) > 100 {
		badRequest(c, "invalid_request", "at most 100 items per import")
		return
	}
	result, err := a.store.Import(c.Request.Context(), scope, c.GetLoginUID(), body.Items)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (a *API) get(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	resource, err := a.store.Get(c.Request.Context(), scope, c.Param("capability_id"))
	if errors.Is(err, ErrNotFound) {
		notFound(c)
		return
	}
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resource})
}

func (a *API) deleteResource(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	if err := a.store.Delete(c.Request.Context(), scope, c.Param("capability_id")); errors.Is(err, ErrNotFound) {
		notFound(c)
		return
	} else if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}})
}

func (a *API) setAuthorization(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	var body struct {
		Status AuthStatus `json:"status"`
	}
	if err := decodeJSON(c, &body, 64<<10); err != nil {
		badRequest(c, "invalid_request", err.Error())
		return
	}
	resource, err := a.store.SetAuthorization(c.Request.Context(), scope, c.Param("capability_id"), body.Status)
	if errors.Is(err, ErrNotFound) {
		notFound(c)
		return
	}
	if err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) {
			badRequest(c, "invalid_authorization", validation.Message)
			return
		}
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resource})
}

func (a *API) listBindings(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	items, err := a.store.ListBindings(c.Request.Context(), scope, c.Param("expert_id"))
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items}})
}

func (a *API) bind(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	var body struct {
		ResourceIDs []string `json:"resource_ids"`
	}
	if err := decodeJSON(c, &body, 1<<20); err != nil {
		badRequest(c, "invalid_request", err.Error())
		return
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(body.ResourceIDs))
	for _, id := range body.ResourceIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		badRequest(c, "invalid_request", "resource_ids are required")
		return
	}
	if len(ids) > 200 {
		badRequest(c, "invalid_request", "at most 200 bindings per request")
		return
	}
	items, err := a.store.Bind(c.Request.Context(), scope, c.Param("expert_id"), ids)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": items}})
}

func (a *API) unbind(c *wkhttp.Context) {
	scope, ok := a.scope(c)
	if !ok {
		return
	}
	if err := a.store.Unbind(c.Request.Context(), scope, c.Param("expert_id"), c.Param("capability_id")); err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{}})
}

func (a *API) scope(c *wkhttp.Context) (Scope, bool) {
	if a.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "database_unavailable", "message": "capability database is not configured"}})
		return Scope{}, false
	}
	spaceID := spacepkg.GetSpaceID(c)
	workspaceID := c.Param("workspace_id")
	if !validScopeID(spaceID) {
		badRequest(c, "invalid_space", "space_id is invalid")
		return Scope{}, false
	}
	if !validScopeID(workspaceID) {
		badRequest(c, "invalid_workspace", "workspace_id is invalid")
		return Scope{}, false
	}
	return Scope{SpaceID: spaceID, WorkspaceID: workspaceID}, true
}

func validScopeID(value string) bool {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r == 0 || r == '\t' || r == '\r' || r == '\n' || r == ' ' {
			return false
		}
	}
	return true
}

func (a *API) fail(c *wkhttp.Context, err error) {
	a.Error("workspace capability request failed", zap.Error(err))
	var validation *ValidationError
	if errors.As(err, &validation) {
		badRequest(c, "invalid_request", validation.Message)
		return
	}
	if errors.Is(err, ErrNotFound) {
		notFound(c)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "capability request failed"}})
}

func decodeJSON(c *wkhttp.Context, target any, limit int64) error {
	if c.Request.Body == nil {
		return fmt.Errorf("request body is required")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("multiple JSON values are not allowed")
	}
	return nil
}

func badRequest(c *wkhttp.Context, code, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": code, "message": message}})
}

func notFound(c *wkhttp.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": "capability not found"}})
}
