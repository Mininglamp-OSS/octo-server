package project

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/obo"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/go-redis/redis"
	"go.uber.org/zap"
)

// Project handlers derive this Action from their trusted route. The Registry
// remains the policy authority that maps it to configured Scopes.
const botProjectOBOAction = "all"

// Bot Project reads bypass the Human Session middleware by design: they use a
// bf_ Bot token and the same OBO kernel as /v1/internal/auth/resolve. The existing
// Project read methods remain the business-permission authority for Subject.
func (p *Project) registerBotProjectRoutes(r *wkhttp.WKHttp) {
	rlRedis := octoredis.NewInstrumentedClient(p.ctx.GetConfig(), func(o *redis.Options) { o.PoolSize = 10 })
	// This unauthenticated boundary is limited per source IP before Bot
	// resolution. Do not reuse per-Bot business quota settings for an IP bucket.
	rlCtx := context.Background()
	ipLimit := r.StrictIPRateLimitMiddleware(rlCtx, rlRedis, "bot_project_read", 30.0/60, 20)
	bot := r.Group("/v1/bot/projects", ipLimit)
	bot.GET("", p.botListProjects)
	bot.GET("/:project_id", p.botGetProject)
	bot.GET("/:project_id/members", p.botListProjectMembers)
}

func (p *Project) botOBOPrincipal(c *wkhttp.Context, action string) (*obo.Principal, bool) {
	c.Writer.Header().Set("Cache-Control", "no-store")
	if c.Query("obo") != "true" {
		respondProjectRequestInvalid(c, "obo")
		return nil, false
	}
	for _, forbidden := range []string{"on_behalf_of", "human_uid", "subject_uid"} {
		if _, exists := c.GetQuery(forbidden); exists {
			respondProjectRequestInvalid(c, forbidden)
			return nil, false
		}
	}
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" || spaceID != c.Query("space_id") {
		respondProjectRequestInvalid(c, "space_id")
		return nil, false
	}
	token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(token, "bf_") {
		httperr.ResponseErrorLWithStatus(c, errSharedAuthRequired, nil, nil)
		return nil, false
	}
	reader := p.oboReader
	if reader == nil {
		reader = obo.DBSnapshotReader{Session: p.db.session}
	}
	resolver := obo.Resolver{Reader: reader, Registry: p.oboRegistry}
	principal, err := resolver.Resolve(c.Request.Context(), obo.Request{
		BotToken: token, Mode: obo.ModeOBO, SpaceID: spaceID, Action: action,
	})
	if err != nil {
		var decision *obo.DecisionError
		if errors.As(err, &decision) {
			switch decision.Code {
			case "invalid_credential":
				httperr.ResponseErrorLWithStatus(c, errSharedAuthRequired, nil, nil)
			case "infra_failure":
				p.Error("Bot Project OBO failed", zap.String("decision_code", decision.Code), zap.String("action", action), zap.Error(err))
				httperr.ResponseErrorLWithStatus(c, errcode.ErrProjectQueryFailed, nil, nil)
			default:
				httperr.ResponseErrorLWithStatus(c, errSharedForbidden, nil, nil)
			}
		} else {
			p.Error("Bot Project OBO failed", zap.String("decision_code", "infra_failure"), zap.String("action", action), zap.Error(err))
			httperr.ResponseErrorLWithStatus(c, errcode.ErrProjectQueryFailed, nil, nil)
		}
		return nil, false
	}
	c.Writer.Header().Set("X-OBO-Decision-ID", principal.Delegation.DecisionID)
	p.Info("Bot Project OBO identity resolved",
		zap.String("actor_uid", principal.Actor.UID), zap.String("subject_uid", principal.Subject.UID),
		zap.String("space_id", principal.Subject.SpaceID), zap.String("action", action),
		zap.Int64("grant_id", principal.Delegation.GrantID), zap.String("decision_id", principal.Delegation.DecisionID))
	return principal, true
}

func botProjectPage(c *wkhttp.Context) (projectReadPage, bool) {
	page := projectReadPage{Limit: projectDefaultPageLimit}
	if raw := c.Query("limit"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 || value > projectMaxPageLimit {
			return page, false
		}
		page.Limit = value
	}
	if raw := c.Query("offset"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 || value > int64(projectMaxPage)*page.Limit {
			return page, false
		}
		page.Offset = value
	}
	return page, true
}

func (p *Project) botListProjects(c *wkhttp.Context) {
	principal, ok := p.botOBOPrincipal(c, botProjectOBOAction)
	if !ok {
		return
	}
	page, valid := botProjectPage(c)
	if !valid {
		respondProjectRequestInvalid(c, "limit|offset")
		return
	}
	result, err := p.readProjects(principal.Subject.SpaceID, principal.Subject.UID, strings.TrimSpace(c.Query("keyword")), page)
	if err != nil {
		p.respondBotProjectReadError(c, err, "Bot 查询项目列表失败", principal.Subject.SpaceID, principal.Subject.UID)
		return
	}
	items := make([]*Resp, 0, len(result.Rows))
	for _, row := range result.Rows {
		items = append(items, p.toResp(&row.Model, row.MyRole, roleNonMember, row.Humans, row.Agents, row.Pinned == 1))
	}
	c.Response(map[string]any{"items": items, "total": result.Total, "limit": page.Limit,
		"offset": page.Offset, "decision_id": principal.Delegation.DecisionID})
}

func (p *Project) botGetProject(c *wkhttp.Context) {
	principal, ok := p.botOBOPrincipal(c, botProjectOBOAction)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	result, err := p.readProject(principal.Subject.SpaceID, projectID, principal.Subject.UID)
	if err != nil {
		p.respondBotProjectReadError(c, err, "Bot 查询项目详情失败", projectID, principal.Subject.UID)
		return
	}
	c.Response(p.toResp(result.Project, result.Role, result.SpaceRole, result.Humans, result.Agents, result.Pinned))
}

func (p *Project) botListProjectMembers(c *wkhttp.Context) {
	principal, ok := p.botOBOPrincipal(c, botProjectOBOAction)
	if !ok {
		return
	}
	page, valid := botProjectPage(c)
	if !valid {
		respondProjectRequestInvalid(c, "limit|offset")
		return
	}
	projectID := strings.TrimSpace(c.Param("project_id"))
	result, err := p.readProjectMembers(principal.Subject.SpaceID, projectID, principal.Subject.UID, page)
	if err != nil {
		p.respondBotProjectReadError(c, err, "Bot 查询项目成员失败", projectID, principal.Subject.UID)
		return
	}
	type publicMember struct {
		UID   string `json:"uid"`
		Name  string `json:"name"`
		Role  int    `json:"role"`
		Robot int    `json:"robot"`
	}
	items := make([]publicMember, 0, len(result.Rows))
	for _, member := range result.Rows {
		items = append(items, publicMember{UID: member.UID, Name: member.Name, Role: member.Role, Robot: member.Robot})
	}
	c.Response(map[string]any{"items": items, "total": result.Total, "limit": page.Limit,
		"offset": page.Offset, "decision_id": principal.Delegation.DecisionID})
}

// These new Bot routes have no legacy 400-on-every-error client contract.
// Preserve Project's localized envelope while using its semantic HTTP status.
func (p *Project) respondBotProjectReadError(c *wkhttp.Context, err error, message, resource, uid string) {
	switch {
	case errors.Is(err, errProjectReadNotFound), errors.Is(err, errProjectGone):
		p.Debug(message+"：项目不可读", zap.String("resource", resource), zap.String("uid", uid))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrProjectNotFound, nil, nil)
	case errors.Is(err, errProjectReadForbidden):
		httperr.ResponseErrorLWithStatus(c, errSharedForbidden, nil, nil)
	default:
		p.Error(message, zap.Error(err), zap.String("resource", resource), zap.String("uid", uid))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrProjectQueryFailed, nil, nil)
	}
}
