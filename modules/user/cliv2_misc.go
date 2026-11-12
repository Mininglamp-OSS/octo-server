package user

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"go.uber.org/zap"
)

type cliv2UserDetailContextKey struct{}

func (u *User) registerCLIV2Routes(source *wkhttp.WKHttp) {
	cliv2.Add(source, cliv2.Route{
		Method: http.MethodGet,
		Path:   "/v2/cli/identity",
		Action: cliv2.ActionIdentityRead,
		User:   u.cliv2Identity,
		Bot:    u.cliv2Identity,
	})
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodGet,
		Path:        "/v2/cli/users/:user_id",
		Action:      cliv2.ActionUsersRead,
		User:        u.cliv2GetUser,
		Bot:         u.cliv2GetUser,
		Middlewares: []wkhttp.HandlerFunc{u.requireCLIV2UserSpaceAccess()},
	})
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodGet,
		Path:        "/v2/cli/space/members",
		Action:      cliv2.ActionSpacesRead,
		User:        u.cliv2SpaceMembers,
		Bot:         u.cliv2SpaceMembers,
		Middlewares: []wkhttp.HandlerFunc{u.requireCLIV2ActorSpaceMember()},
	})
}

func (u *User) cliv2Identity(c *wkhttp.Context) {
	principal := cliv2.PrincipalFrom(c)
	c.Header("Cache-Control", "no-store")
	cliv2.Respond(c, struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
		BotID    string `json:"bot_id"`
		SpaceID  string `json:"space_id"`
	}{
		UserID:   principal.UserID,
		UserName: principal.UserName,
		BotID:    principal.BotID,
		SpaceID:  principal.SpaceID,
	})
}

// requireCLIV2TokenOwnerSpaceMember verifies that the Token owner is still an
// active member of the bound Space. A user Token resolves the actor to the
// owner, so the actor check that ran before this helper already covers that
// same subject and no duplicate query is issued. A bound Bot acts as the Bot,
// but the Bot's access must not outlive the owner's Space membership.
func (u *User) requireCLIV2TokenOwnerSpaceMember(c *wkhttp.Context, principal cliv2.Principal, actorID string) bool {
	if principal.UserID == "" || principal.UserID == actorID {
		return true
	}
	member, err := spacepkg.CheckMembership(u.ctx.DB(), principal.SpaceID, principal.UserID)
	if err != nil {
		u.Error("CLI v2 Token owner Space membership check failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		c.Abort()
		return false
	}
	if !member {
		cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
		c.Abort()
		return false
	}
	return true
}

func (u *User) requireCLIV2ActorSpaceMember() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		principal := cliv2.PrincipalFrom(c)
		actorID := c.GetLoginUID()
		if principal.SpaceID == "" || actorID == "" {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		member, err := spacepkg.CheckMembership(u.ctx.DB(), principal.SpaceID, actorID)
		if err != nil {
			u.Error("CLI v2 Space membership check failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		if !member {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		if !u.requireCLIV2TokenOwnerSpaceMember(c, principal, actorID) {
			return
		}
		c.Next()
	}
}

func (u *User) requireCLIV2UserSpaceAccess() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		principal := cliv2.PrincipalFrom(c)
		actorID := c.GetLoginUID()
		if principal.SpaceID == "" || actorID == "" {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		actorMember, err := spacepkg.CheckMembership(u.ctx.DB(), principal.SpaceID, actorID)
		if err != nil {
			u.Error("CLI v2 actor Space membership check failed", zap.Error(err))
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		if !actorMember {
			cliv2.Fail(c, http.StatusForbidden, "FORBIDDEN", "Access denied.")
			c.Abort()
			return
		}
		// The owner boundary is checked before target hiding so a revoked owner
		// always receives the authorization error, not a target-dependent one.
		if !u.requireCLIV2TokenOwnerSpaceMember(c, principal, actorID) {
			return
		}

		target := c.Param("user_id")
		if target == "" {
			cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
			c.Abort()
			return
		}
		// The actor and owner were already verified above; only query a third
		// distinct subject.
		if target != actorID && target != principal.UserID && !spacepkg.IsSystemBot(target) {
			member, err := spacepkg.CheckMembership(u.ctx.DB(), principal.SpaceID, target)
			if err != nil {
				u.Error("CLI v2 target Space membership check failed", zap.Error(err))
				cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
				c.Abort()
				return
			}
			if !member {
				cliv2.Fail(c, http.StatusNotFound, "NOT_FOUND", "Resource not found.")
				c.Abort()
				return
			}
		}

		// The legacy user-detail handler has an optional group-scoped enrichment.
		// It is not part of the CLI v2 user lookup and is removed rather than
		// allowing caller-selected group metadata to cross the bound Space.
		query := c.Request.URL.Query()
		query.Del("group_no")
		c.Request.URL.RawQuery = query.Encode()
		c.Next()
	}
}

func (u *User) cliv2GetUser(c *wkhttp.Context) {
	ctx := context.WithValue(c.Request.Context(), cliv2UserDetailContextKey{}, true)
	c.Request = c.Request.WithContext(ctx)
	u.get(c)
}

func (u *User) cliv2SpaceMembers(c *wkhttp.Context) {
	spaceID := cliv2.PrincipalFrom(c).SpaceID
	keyword := strings.TrimSpace(c.Query("keyword"))
	if len(keyword) > 128 {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request parameters.")
		return
	}

	page := 1
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request parameters.")
			return
		}
		page = parsed
	}
	pageSize := 20
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request parameters.")
			return
		}
		pageSize = parsed
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/pageSize {
		cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Invalid request parameters.")
		return
	}
	offset := (page - 1) * pageSize

	type memberInfo struct {
		UID   string `json:"uid"`
		Name  string `json:"name"`
		Robot int    `json:"robot"`
	}
	base := "FROM space_member sm LEFT JOIN `user` u ON sm.uid=u.uid WHERE sm.space_id=? AND sm.status=1"
	args := []any{spaceID}
	if keyword != "" {
		escaped := strings.ReplaceAll(keyword, "\\", "\\\\")
		escaped = strings.ReplaceAll(escaped, "%", "\\%")
		escaped = strings.ReplaceAll(escaped, "_", "\\_")
		base += " AND u.name LIKE ?"
		args = append(args, "%"+escaped+"%")
	}

	var total int64
	_, err := u.ctx.DB().SelectBySql("SELECT COUNT(*) "+base, args...).Load(&total)
	if err != nil {
		u.Error("CLI v2 count Space members failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return
	}

	members := make([]memberInfo, 0)
	queryArgs := append(append([]any(nil), args...), pageSize, offset)
	_, err = u.ctx.DB().SelectBySql(
		"SELECT sm.uid, IFNULL(u.name,'') AS name, IFNULL(u.robot,0) AS robot "+base+" ORDER BY sm.id LIMIT ? OFFSET ?",
		queryArgs...,
	).Load(&members)
	if err != nil {
		u.Error("CLI v2 query Space members failed", zap.Error(err))
		cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"data": members,
		"pagination": map[string]any{
			"total": total, "page": page, "page_size": pageSize,
		},
	})
}

func isCLIV2UserDetail(c *wkhttp.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	v, _ := c.Request.Context().Value(cliv2UserDetailContextKey{}).(bool)
	return v
}

func respondUserDetail(c *wkhttp.Context, data any) {
	if isCLIV2UserDetail(c) {
		cliv2.Respond(c, data)
		return
	}
	c.Response(data)
}

func failUserDetail(c *wkhttp.Context, status int, code, message string, legacy func()) {
	if isCLIV2UserDetail(c) {
		cliv2.Fail(c, status, code, message)
		return
	}
	legacy()
}
