package space

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// Per-Space onboarding welcome config CRUD (task
// space-welcome-per-space-admin-crud). A Space admin (Role>=1) manages ONE
// welcome config for their own Space:
//
//	GET    /v1/space/:space_id/welcome  — read the per-Space config + what is in effect
//	PUT    /v1/space/:space_id/welcome  — create/update (增/改), prospective-validated
//	DELETE /v1/space/:space_id/welcome  — hard-delete (删); reverts to global fallback
//
// Authorization mirrors searchMembers: the caller must be an active member of
// the path Space with Role>=admin, and the Space must be active. A request can
// only ever affect the path :space_id (space isolation). All errors go through
// the httperr i18n envelope. The routes carry SharedUIDRateLimiter (see Route).

// welcomeConfigDBTimeout bounds each config DB call on the CRUD path.
const welcomeConfigDBTimeout = 3 * time.Second

// putWelcomeReq is the PUT body. Pointer fields distinguish "omitted" (keep the
// current value) from an explicit value, so a partial update is a valid patch
// merged onto the existing row (prospective validation).
type putWelcomeReq struct {
	Enabled    *bool   `json:"enabled"`
	ActiveFrom *string `json:"active_from"` // RFC3339 UTC
	Message    *string `json:"message"`
}

// welcomeEffectiveResp describes the config actually in effect for the Space,
// after resolving per-Space over global fallback.
type welcomeEffectiveResp struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"` // space | global | none
	Message string `json:"message"`
}

// welcomeConfigResp is the GET/PUT response.
type welcomeConfigResp struct {
	Configured bool                 `json:"configured"` // a per-Space row exists
	Enabled    bool                 `json:"enabled"`
	ActiveFrom string               `json:"active_from"`
	Message    string               `json:"message"`
	UpdatedBy  string               `json:"updated_by,omitempty"`
	UpdatedAt  string               `json:"updated_at,omitempty"`
	Effective  welcomeEffectiveResp `json:"effective"`
}

// authorizeSpaceAdmin gates a per-Space welcome request: the Space must be
// active and the caller must be an admin/owner (Role>=1). It reuses the module's
// checkSpaceActive + requireSpaceAdmin helpers (both write the localized
// response and report "handled" on failure). A request can only ever affect the
// path :space_id, so space isolation is enforced by construction. Returns the
// caller uid and ok=false when a response was already written.
func (s *Space) authorizeSpaceAdmin(c *wkhttp.Context, spaceId string) (loginUID string, ok bool) {
	loginUID = c.GetLoginUID()
	if s.checkSpaceActive(c, spaceId) {
		return "", false
	}
	if s.requireSpaceAdmin(c, spaceId, loginUID) {
		return "", false
	}
	return loginUID, true
}

// getWelcome returns the per-Space config (if any) plus the effective config so
// the admin UI can show whether a global fallback is in play.
func (s *Space) getWelcome(c *wkhttp.Context) {
	spaceId := c.Param("space_id")
	if _, ok := s.authorizeSpaceAdmin(c, spaceId); !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), welcomeConfigDBTimeout)
	defer cancel()
	row, found, err := s.welcomeStore.Get(ctx, spaceId)
	if err != nil {
		s.Error("查询空间欢迎语配置失败", zap.Error(err), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	eff := commonmod.ResolveEffectiveSpaceWelcome(row, found, s.settings.SpaceWelcomeConfig(), spaceId)
	c.Response(buildWelcomeResp(row, found, eff))
}

// putWelcome upserts the per-Space config. Partial updates merge onto the
// existing row (prospective validation): the patch is accepted iff the merged
// combination is valid. The target Space is the path Space (already active).
func (s *Space) putWelcome(c *wkhttp.Context) {
	spaceId := c.Param("space_id")
	loginUID, ok := s.authorizeSpaceAdmin(c, spaceId)
	if !ok {
		return
	}

	var req putWelcomeReq
	if err := c.BindJSON(&req); err != nil {
		respondSpaceRequestInvalid(c, "")
		return
	}
	if req.Enabled == nil && req.ActiveFrom == nil && req.Message == nil {
		respondSpaceRequestInvalid(c, "")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), welcomeConfigDBTimeout)
	defer cancel()

	// Start from the existing row (or defaults) and overlay the provided fields.
	row, found, err := s.welcomeStore.Get(ctx, spaceId)
	if err != nil {
		s.Error("查询空间欢迎语配置失败", zap.Error(err), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	prospective := commonmod.SpaceWelcomeSpaceConfig{SpaceID: spaceId}
	if found {
		prospective = *row
	}
	prospective.SpaceID = spaceId
	if req.Enabled != nil {
		prospective.Enabled = *req.Enabled
	}
	if req.ActiveFrom != nil {
		prospective.ActiveFromRaw = strings.TrimSpace(*req.ActiveFrom)
	}
	if req.Message != nil {
		// Preserve internal newlines verbatim (plain text, no markdown); only the
		// composite validator trims for the non-empty check.
		prospective.Message = *req.Message
	}

	// Column guard: bound the message length regardless of enabled so an
	// oversized body is rejected even for a disabled config (protects the
	// VARCHAR(2000) column and avoids silent truncation).
	if utf8.RuneCountInString(prospective.Message) > commonmod.SpaceWelcomeMessageMaxRunes {
		respondSpaceFieldTooLong(c, "message", commonmod.SpaceWelcomeMessageMaxRunes)
		return
	}

	// Composite validation via the shared validator. nil isActiveSpace: the
	// target Space is the path Space, already confirmed active by
	// requireSpaceAdmin, so no second existence check is needed.
	eff := commonmod.SpaceWelcomeConfig{
		Enabled:       prospective.Enabled,
		SpaceID:       spaceId,
		ActiveFromRaw: prospective.ActiveFromRaw,
		Message:       prospective.Message,
	}
	if field, _ := commonmod.ValidateSpaceWelcomeCombination(eff, nil); field != "" {
		httperr.ResponseErrorL(c, errcode.ErrSpaceWelcomeConfigInvalid, nil, i18n.Details{"field": welcomeClientField(field)})
		return
	}

	prospective.UpdatedBy = loginUID
	if err := s.welcomeStore.Upsert(ctx, prospective, time.Now().UTC()); err != nil {
		s.Error("写入空间欢迎语配置失败", zap.Error(err), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	s.Info("空间欢迎语配置已更新", zap.String("operator_uid", loginUID), zap.String("space_id", spaceId), zap.Bool("enabled", prospective.Enabled))

	// Re-read so the response carries the persisted updated_at / effective view.
	row, found, err = s.welcomeStore.Get(ctx, spaceId)
	if err != nil {
		s.Error("回读空间欢迎语配置失败", zap.Error(err), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceQueryFailed, nil, nil)
		return
	}
	eff2 := commonmod.ResolveEffectiveSpaceWelcome(row, found, s.settings.SpaceWelcomeConfig(), spaceId)
	c.Response(buildWelcomeResp(row, found, eff2))
}

// deleteWelcome hard-deletes the per-Space config; the Space then reverts to the
// platform-global fallback (if it names this Space) or off. Idempotent: deleting
// an absent config returns deleted=false with 200.
func (s *Space) deleteWelcome(c *wkhttp.Context) {
	spaceId := c.Param("space_id")
	loginUID, ok := s.authorizeSpaceAdmin(c, spaceId)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), welcomeConfigDBTimeout)
	defer cancel()
	deleted, err := s.welcomeStore.Delete(ctx, spaceId)
	if err != nil {
		s.Error("删除空间欢迎语配置失败", zap.Error(err), zap.String("space_id", spaceId))
		httperr.ResponseErrorL(c, errcode.ErrSpaceStoreFailed, nil, nil)
		return
	}
	s.Info("空间欢迎语配置已删除", zap.String("operator_uid", loginUID), zap.String("space_id", spaceId), zap.Bool("deleted", deleted))
	c.Response(map[string]interface{}{"deleted": deleted})
}

// buildWelcomeResp assembles the response from the per-Space row (if present)
// and the resolved effective config.
func buildWelcomeResp(row *commonmod.SpaceWelcomeSpaceConfig, found bool, eff commonmod.SpaceWelcomeConfig) welcomeConfigResp {
	resp := welcomeConfigResp{Configured: found}
	if found {
		resp.Enabled = row.Enabled
		resp.ActiveFrom = row.ActiveFromRaw
		resp.Message = row.Message
		resp.UpdatedBy = row.UpdatedBy
		if !row.UpdatedAt.IsZero() {
			resp.UpdatedAt = row.UpdatedAt.String()
		}
	}
	resp.Effective = welcomeEffectiveResp{
		Enabled: eff.Enabled,
		Message: eff.Message,
		Source:  welcomeSource(found, eff),
	}
	return resp
}

// welcomeSource names where the effective config comes from: a present per-Space
// row is always authoritative ("space"); otherwise an active global fallback is
// "global"; otherwise "none".
func welcomeSource(found bool, eff commonmod.SpaceWelcomeConfig) string {
	if found {
		return "space"
	}
	if eff.Enabled {
		return "global"
	}
	return "none"
}

// welcomeClientField maps the shared validator's offending key
// (space_welcome_*) to the client-facing PUT field name.
func welcomeClientField(field string) string {
	switch field {
	case "space_welcome_active_from":
		return "active_from"
	case "space_welcome_message":
		return "message"
	case "space_welcome_space_id":
		return "space_id"
	default:
		return field
	}
}
