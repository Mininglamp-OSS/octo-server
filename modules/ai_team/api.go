package ai_team

import (
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

type API struct {
	ctx     *config.Context
	service *Service
	log.Log
}

func New(ctx *config.Context) *API {
	return &API{ctx: ctx, service: NewService(ctx), Log: log.NewTLog("AITeam")}
}

func (a *API) Route(r *wkhttp.WKHttp) {
	g := r.Group("/v1/ai-team",
		a.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, a.ctx),
		spacepkg.SpaceMiddleware(a.ctx),
	)
	g.GET("/agents", a.requireEnabled, a.listAgents)
	g.POST("/agents/:bot_id", a.requireEnabled, a.addAgent)
	g.DELETE("/agents/:bot_id", a.requireEnabled, a.removeAgent)
	g.GET("/agents/:bot_id/sessions", a.requireEnabled, a.listSessions)
	g.POST("/agents/:bot_id/sessions", a.requireEnabled, a.createSession)
	g.GET("/sessions/:short_id", a.requireEnabled, a.getSession)
	g.PUT("/sessions/:short_id", a.requireEnabled, a.renameSession)
	g.DELETE("/sessions/:short_id", a.requireEnabled, a.deleteSession)
	g.PUT("/sessions/:short_id/setting", a.requireEnabled, a.updateSessionSetting)
	g.POST("/sessions/:short_id/archive", a.requireEnabled, a.archiveSession)
	g.POST("/sessions/:short_id/unarchive", a.requireEnabled, a.unarchiveSession)
}

func (a *API) requireEnabled(c *wkhttp.Context) {
	if !aiteampkg.Enabled() {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrAITeamDisabled, nil, nil)
		c.Abort()
		return
	}
	if spacepkg.GetSpaceID(c) == "" {
		respondInvalid(c, "space_id")
		c.Abort()
		return
	}
	c.Next()
}

func (a *API) addAgent(c *wkhttp.Context) {
	agent, err := a.service.AddAgent(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("bot_id")))
	if err != nil {
		a.respond(c, "add agent", err)
		return
	}
	c.Response(agent)
}

func (a *API) removeAgent(c *wkhttp.Context) {
	err := a.service.RemoveAgent(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("bot_id")))
	if err != nil {
		a.respond(c, "remove agent", err)
		return
	}
	c.Response(map[string]interface{}{"ok": true})
}

func (a *API) listAgents(c *wkhttp.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	beforeID, _ := strconv.ParseInt(c.Query("cursor"), 10, 64)
	page, err := a.service.ListAgents(spacepkg.GetSpaceID(c), c.GetLoginUID(), beforeID, limit)
	if err != nil {
		a.respond(c, "list agents", err)
		return
	}
	c.Response(page)
}

func (a *API) createSession(c *wkhttp.Context) {
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" || len(idempotencyKey) > maxIdempotencyKey {
		respondInvalid(c, "Idempotency-Key")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		respondInvalid(c, "body")
		return
	}
	if len([]rune(req.Name)) > 100 {
		respondInvalid(c, "name")
		return
	}
	session, err := a.service.CreateSession(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("bot_id")), idempotencyKey, req.Name)
	if err != nil {
		a.respond(c, "create session", err)
		return
	}
	c.Response(session)
}

func (a *API) listSessions(c *wkhttp.Context) {
	statuses, err := normalizeStatuses(c.Query("status"))
	if err != nil {
		respondInvalid(c, "status")
		return
	}
	pageIndex, _ := strconv.Atoi(c.Query("page_index"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	page, err := a.service.ListSessions(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("bot_id")), statuses, pageIndex, pageSize)
	if err != nil {
		a.respond(c, "list sessions", err)
		return
	}
	c.Response(page)
}

func (a *API) getSession(c *wkhttp.Context) {
	session, err := a.service.GetSession(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("short_id")))
	if err != nil {
		a.respond(c, "get session", err)
		return
	}
	c.Response(session)
}

func (a *API) renameSession(c *wkhttp.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondInvalid(c, "body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 100 {
		respondInvalid(c, "name")
		return
	}
	if err := a.service.RenameSession(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("short_id")), req.Name); err != nil {
		a.respond(c, "rename session", err)
		return
	}
	c.Response(map[string]interface{}{"ok": true})
}

func (a *API) updateSessionSetting(c *wkhttp.Context) {
	var req struct {
		Mute *int `json:"mute"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Mute == nil || (*req.Mute != 0 && *req.Mute != 1) {
		respondInvalid(c, "mute")
		return
	}
	if err := a.service.UpdateSessionSetting(
		spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("short_id")),
		map[string]interface{}{"mute": float64(*req.Mute)},
	); err != nil {
		a.respond(c, "change session setting", err)
		return
	}
	c.Response(map[string]interface{}{"ok": true})
}

func (a *API) deleteSession(c *wkhttp.Context) {
	if err := a.service.DeleteSession(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("short_id"))); err != nil {
		a.respond(c, "delete session", err)
		return
	}
	c.Response(map[string]interface{}{"ok": true})
}

func (a *API) archiveSession(c *wkhttp.Context) {
	a.setArchive(c, true)
}

func (a *API) unarchiveSession(c *wkhttp.Context) {
	a.setArchive(c, false)
}

func (a *API) setArchive(c *wkhttp.Context, archive bool) {
	if err := a.service.ArchiveSession(spacepkg.GetSpaceID(c), c.GetLoginUID(), strings.TrimSpace(c.Param("short_id")), archive); err != nil {
		a.respond(c, "change session archive", err)
		return
	}
	c.Response(map[string]interface{}{"ok": true})
}

func (a *API) respond(c *wkhttp.Context, operation string, err error) {
	if !errors.Is(err, errForbidden) && !errors.Is(err, errNotFound) && !errors.Is(err, errIdempotencyConflict) {
		a.Error("AI team "+operation+" failed", zap.Error(err), zap.String("uid", c.GetLoginUID()), zap.String("space_id", spacepkg.GetSpaceID(c)))
	}
	respondServiceError(c, err)
}
