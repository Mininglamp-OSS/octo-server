package user

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/obo"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const resolveMaxBodyBytes = 8192

// authResolveBot is the Bot identity wire protocol. Its fixed
// {code,request_id} envelope is intentionally separate from the user-facing
// localized API errors and from the legacy verify-bot response.
func (u *User) authResolveBot(c *wkhttp.Context) {
	c.Writer.Header().Set("Cache-Control", "no-store")
	if c.Request.ContentLength > resolveMaxBodyBytes {
		resolveError(c, "invalid_request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, resolveMaxBodyBytes+1))
	if err != nil || len(body) > resolveMaxBodyBytes {
		resolveError(c, "invalid_request", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var req obo.Request
	if err := decoder.Decode(&req); err != nil {
		resolveError(c, "invalid_request", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		resolveError(c, "invalid_request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.BotToken) != req.BotToken || strings.TrimSpace(req.SpaceID) != req.SpaceID {
		resolveError(c, "invalid_request", http.StatusBadRequest)
		return
	}
	resolver := obo.Resolver{Reader: u.oboReader, Registry: u.oboRegistry}
	principal, err := resolver.Resolve(c.Request.Context(), req)
	if err != nil {
		var decision *obo.DecisionError
		if !errors.As(err, &decision) {
			u.Error("Bot Resolve failed", zap.String("decision_code", "infra_failure"))
			resolveError(c, "infra_failure", http.StatusServiceUnavailable)
			return
		}
		if decision.Status >= 500 {
			u.Error("Bot Resolve failed", zap.String("decision_code", decision.Code))
		}
		resolveError(c, decision.Code, decision.Status)
		return
	}
	if principal.Delegation != nil {
		fields := []zap.Field{
			zap.String("actor_uid", principal.Actor.UID),
			zap.String("subject_uid", principal.Subject.UID), zap.String("space_id", principal.Subject.SpaceID),
			zap.String("action", principal.Delegation.Action), zap.Int64("grant_id", principal.Delegation.GrantID),
			zap.String("decision_id", principal.Delegation.DecisionID),
		}
		if req.Resource != nil {
			fields = append(fields, zap.String("resource_type", req.Resource.Type), zap.String("resource_id", req.Resource.ID))
		}
		u.Info("Bot OBO identity resolved", fields...)
	} else {
		u.Info("Bot identity resolved", zap.String("mode", string(principal.Mode)),
			zap.String("actor_uid", principal.Actor.UID), zap.String("space_id", principal.Actor.SpaceID))
	}
	c.Response(principal)
}

func resolveError(c *wkhttp.Context, code string, status int) {
	// This internal protocol intentionally has a stable non-localized envelope;
	// do not expose it as a user-facing error or echo secret-bearing input.
	c.AbortWithStatusJSON(status, gin.H{"code": code, "request_id": util.GenerUUID()})
}
