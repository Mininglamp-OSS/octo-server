package group

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"go.uber.org/zap"
)

// Per-group 入群欢迎语 CRUD (task group-welcome-message). A group creator/manager
// manages ONE welcome config for their own group:
//
//	GET    /v1/groups/:group_no/welcome  — read the config + what is in effect
//	PUT    /v1/groups/:group_no/welcome  — create/update (增/改), prospective-validated
//	DELETE /v1/groups/:group_no/welcome  — hard-delete (删); reverts to off
//
// Authorization: the group must be active (not disbanded) and the caller must be
// creator/manager (role ∈ {creator, manager}); a request can only ever affect the
// path :group_no (isolation). There is NO platform-global fallback (unlike the
// per-Space welcome). All errors go through the httperr i18n envelope with
// existing group error codes. Routes carry SharedUIDRateLimiter (see Route).

// groupWelcomeConfigDBTimeout bounds each config DB call on the CRUD path.
const groupWelcomeConfigDBTimeout = 3 * time.Second

// errGroupWelcomeValidation is the sentinel a putWelcome merge closure returns
// after it has already written a localized validation response; UpsertMerged
// rolls the transaction back and putWelcome then just returns.
var errGroupWelcomeValidation = errors.New("group welcome validation failed")

// putGroupWelcomeReq is the PUT body. Pointer fields distinguish "omitted" (keep
// the current value) from an explicit value, so a partial update is a valid patch
// merged onto the existing row (prospective validation).
type putGroupWelcomeReq struct {
	Enabled    *bool   `json:"enabled"`
	ActiveFrom *string `json:"active_from"` // RFC3339 UTC
	Message    *string `json:"message"`
}

// groupWelcomeEffectiveResp describes the config actually in effect for the group
// (per-group row iff present AND enabled; no global fallback).
type groupWelcomeEffectiveResp struct {
	Enabled    bool   `json:"enabled"`
	Source     string `json:"source"`      // group | none
	ActiveFrom string `json:"active_from"` // effective RFC3339 window start ("" when off)
	Message    string `json:"message"`
}

// groupWelcomeConfigResp is the GET/PUT response.
type groupWelcomeConfigResp struct {
	Configured bool                      `json:"configured"` // a per-group row exists
	Enabled    bool                      `json:"enabled"`
	ActiveFrom string                    `json:"active_from"`
	Message    string                    `json:"message"`
	UpdatedBy  string                    `json:"updated_by,omitempty"`
	UpdatedAt  string                    `json:"updated_at,omitempty"`
	Effective  groupWelcomeEffectiveResp `json:"effective"`
}

// authorizeGroupWelcomeAdmin gates a per-group welcome request: the group must be
// active (not disbanded) and the caller must be creator/manager. Reuses
// getGroupInfo (active check) + QueryIsGroupManagerOrCreator (admin check). A
// request can only ever affect the path :group_no, so isolation holds by
// construction. Returns the caller uid and ok=false when a response was written.
func (g *Group) authorizeGroupWelcomeAdmin(c *wkhttp.Context, groupNo string) (loginUID string, ok bool) {
	loginUID = c.GetLoginUID()
	group, err := g.getGroupInfo(groupNo)
	if err != nil {
		respondGroupInfoError(c, err)
		return "", false
	}
	// Require a Normal group (parity with the space welcome's checkSpaceActive):
	// getGroupInfo already rejects Disband(2); also reject Disabled(0) so an
	// admin-disabled group's welcome config cannot be read/written. Same
	// not-found response as a disbanded group (no status-reason leak).
	if group.Status != GroupStatusNormal {
		respondGroupInfoError(c, errGroupInfoNotFound)
		return "", false
	}
	isAdmin, err := g.db.QueryIsGroupManagerOrCreator(groupNo, loginUID)
	if err != nil {
		g.Error("查询群管理员身份失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return "", false
	}
	if !isAdmin {
		// Anti-enumeration: a non-member and an under-privileged member both get
		// the same creator/manager-only 403 (no privilege-reason leak).
		httperr.ResponseErrorL(c, errcode.ErrGroupCreatorOrManagerOnly, nil, nil)
		return "", false
	}
	return loginUID, true
}

// getWelcome returns the per-group config (if any) plus what is in effect.
func (g *Group) getWelcome(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	if _, ok := g.authorizeGroupWelcomeAdmin(c, groupNo); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), groupWelcomeConfigDBTimeout)
	defer cancel()
	row, found, err := g.welcomeStore.Get(ctx, groupNo)
	if err != nil {
		g.Error("查询群欢迎语配置失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupQueryFailed, nil, nil)
		return
	}
	c.Response(buildGroupWelcomeResp(row, found))
}

// putWelcome upserts the per-group config. Partial updates merge onto the
// existing row (prospective validation). Concurrent PUTs are serialized by the
// store's insert-then-lock UpsertMerged.
func (g *Group) putWelcome(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID, ok := g.authorizeGroupWelcomeAdmin(c, groupNo)
	if !ok {
		return
	}

	var req putGroupWelcomeReq
	if err := c.BindJSON(&req); err != nil {
		respondGroupRequestInvalid(c, "")
		return
	}
	if req.Enabled == nil && req.ActiveFrom == nil && req.Message == nil {
		respondGroupRequestInvalid(c, "")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), groupWelcomeConfigDBTimeout)
	defer cancel()

	// Read-merge-write under a row lock (see store.UpsertMerged) so two managers
	// PUTting the same group concurrently can't lose each other's fields.
	// Validation runs inside the merge and, on failure, writes the localized
	// response and returns errGroupWelcomeValidation so the tx aborts (no write).
	persisted, err := g.welcomeStore.UpsertMerged(ctx, groupNo, time.Now().UTC(),
		func(cur *commonmod.GroupWelcomeConfig, found bool) (commonmod.GroupWelcomeConfig, error) {
			prospective := commonmod.GroupWelcomeConfig{GroupNo: groupNo}
			if found {
				prospective = *cur
			}
			prospective.GroupNo = groupNo
			if req.Enabled != nil {
				prospective.Enabled = *req.Enabled
			}
			if req.ActiveFrom != nil {
				prospective.ActiveFromRaw = strings.TrimSpace(*req.ActiveFrom)
			}
			if req.Message != nil {
				// Preserve internal newlines verbatim (plain text, no markdown);
				// only the combination validator trims for the non-empty check.
				prospective.Message = *req.Message
			}

			// Column-width guards run regardless of enabled (clean 400, not a
			// driver 500 / silent truncation).
			if utf8.RuneCountInString(prospective.Message) > commonmod.GroupWelcomeMessageMaxRunes {
				respondGroupRequestInvalid(c, "message")
				return commonmod.GroupWelcomeConfig{}, errGroupWelcomeValidation
			}
			if utf8.RuneCountInString(prospective.ActiveFromRaw) > commonmod.GroupWelcomeActiveFromMaxLen {
				respondGroupRequestInvalid(c, "active_from")
				return commonmod.GroupWelcomeConfig{}, errGroupWelcomeValidation
			}
			if field := commonmod.ValidateGroupWelcomeCombination(prospective); field != "" {
				respondGroupRequestInvalid(c, field)
				return commonmod.GroupWelcomeConfig{}, errGroupWelcomeValidation
			}

			prospective.UpdatedBy = loginUID
			return prospective, nil
		})
	if err != nil {
		if errors.Is(err, errGroupWelcomeValidation) {
			return // the merge already wrote the localized validation response
		}
		g.Error("写入群欢迎语配置失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.Info("群欢迎语配置已更新", zap.String("operator_uid", loginUID), zap.String("group_no", groupNo), zap.Bool("enabled", persisted.Enabled))
	c.Response(buildGroupWelcomeResp(persisted, true))
}

// deleteWelcome hard-deletes the per-group config; the group then reverts to off
// (no fallback). Idempotent: deleting an absent config returns deleted=false 200.
func (g *Group) deleteWelcome(c *wkhttp.Context) {
	groupNo := c.Param("group_no")
	loginUID, ok := g.authorizeGroupWelcomeAdmin(c, groupNo)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), groupWelcomeConfigDBTimeout)
	defer cancel()
	deleted, err := g.welcomeStore.Delete(ctx, groupNo)
	if err != nil {
		g.Error("删除群欢迎语配置失败", zap.Error(err), zap.String("group_no", groupNo))
		httperr.ResponseErrorL(c, errcode.ErrGroupStoreFailed, nil, nil)
		return
	}
	g.Info("群欢迎语配置已删除", zap.String("operator_uid", loginUID), zap.String("group_no", groupNo), zap.Bool("deleted", deleted))
	c.Response(map[string]interface{}{"deleted": deleted})
}

// buildGroupWelcomeResp assembles the response from the per-group row (if any).
// Without a global fallback, the effective config is the row iff enabled.
func buildGroupWelcomeResp(row *commonmod.GroupWelcomeConfig, found bool) groupWelcomeConfigResp {
	resp := groupWelcomeConfigResp{Configured: found}
	eff := groupWelcomeEffectiveResp{Source: "none"}
	if found && row != nil {
		resp.Enabled = row.Enabled
		resp.ActiveFrom = row.ActiveFromRaw
		resp.Message = row.Message
		resp.UpdatedBy = row.UpdatedBy
		if !row.UpdatedAt.IsZero() {
			// Stable RFC3339 (UTC); updated_at is a second-precision DATETIME.
			resp.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if row.Enabled {
			eff.Enabled = true
			eff.Source = "group"
			eff.ActiveFrom = row.ActiveFromRaw
			eff.Message = row.Message
		}
	}
	resp.Effective = eff
	return resp
}
