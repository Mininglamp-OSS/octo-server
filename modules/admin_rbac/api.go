package adminrbac

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

type API struct {
	ctx *config.Context
	svc *Service
}

func New(ctx *config.Context) *API {
	store := NewStore(ctx.DB())
	permissionCache := NewPermissionCache(ctx.Cache())
	return &API{ctx: ctx, svc: NewService(store, permissionCache)}
}

func (a *API) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/manager/rbac", a.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, a.ctx))
	{
		auth.GET("/roles", a.listRoles)
		auth.POST("/roles", a.createRole)
		auth.PUT("/roles/:role_key", a.updateRole)
		auth.GET("/roles/:role_key/permissions", a.listRolePermissions)
		auth.PUT("/roles/:role_key/permissions/:permission_key", a.grantRolePermission)
		auth.DELETE("/roles/:role_key/permissions/:permission_key", a.revokeRolePermission)
		auth.GET("/users/:uid/roles", a.listUserRoles)
		auth.PUT("/users/:uid/roles/:role_key", a.grantUserRole)
		auth.DELETE("/users/:uid/roles/:role_key", a.revokeUserRole)
		auth.GET("/users/:uid/effective-permissions", a.effectivePermissions)
	}
}

type createRoleRequest struct {
	RoleKey     string `json:"role_key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type updateRoleRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      *int   `json:"status"`
}

type rolePermissionsResponse struct {
	RoleKey     string   `json:"role_key"`
	Permissions []string `json:"permissions"`
}

func (a *API) requireManager(c *wkhttp.Context) bool {
	if err := c.CheckLoginRole(); err != nil {
		respondRBACError(c, errcode.ErrSharedForbidden)
		return false
	}
	return true
}

func (a *API) requireSuperAdmin(c *wkhttp.Context) bool {
	if err := c.CheckLoginRoleIsSuperAdmin(); err != nil {
		respondRBACError(c, errcode.ErrSharedForbidden)
		return false
	}
	return true
}

func (a *API) listRoles(c *wkhttp.Context) {
	if !a.requireManager(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	roles, err := a.svc.ListRoles()
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(roles)
}

func (a *API) createRole(c *wkhttp.Context) {
	if !a.requireSuperAdmin(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	var req createRoleRequest
	if err := bindStrictJSON(c, &req); err != nil {
		respondRBACError(c, errcode.ErrSharedParamInvalid)
		return
	}
	role, err := a.svc.CreateRole(req.RoleKey, req.Name, req.Description)
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(role)
}

func (a *API) updateRole(c *wkhttp.Context) {
	if !a.requireSuperAdmin(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	roleKey := strings.TrimSpace(c.Param("role_key"))
	var req updateRoleRequest
	if err := bindStrictJSON(c, &req); err != nil {
		respondRBACError(c, errcode.ErrSharedParamInvalid)
		return
	}
	role, err := a.svc.UpdateRole(roleKey, req.Name, req.Description, req.Status)
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(role)
}

func (a *API) listRolePermissions(c *wkhttp.Context) {
	if !a.requireManager(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	roleKey := strings.TrimSpace(c.Param("role_key"))
	permissions, err := a.svc.RolePermissions(roleKey)
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(&rolePermissionsResponse{RoleKey: roleKey, Permissions: permissions})
}

func (a *API) grantRolePermission(c *wkhttp.Context) {
	a.changeRolePermission(c, true)
}

func (a *API) revokeRolePermission(c *wkhttp.Context) {
	a.changeRolePermission(c, false)
}

func (a *API) changeRolePermission(c *wkhttp.Context, bind bool) {
	if !a.requireSuperAdmin(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	if err := rejectUnexpectedBody(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	if err := a.svc.ChangeRolePermission(c.Param("role_key"), c.Param("permission_key"), bind); err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.ResponseOK()
}

func (a *API) listUserRoles(c *wkhttp.Context) {
	if !a.requireManager(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	roles, err := a.svc.UserRoles(c.Param("uid"))
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(roles)
}

func (a *API) grantUserRole(c *wkhttp.Context) {
	a.changeUserRole(c, true)
}

func (a *API) revokeUserRole(c *wkhttp.Context) {
	a.changeUserRole(c, false)
}

func (a *API) changeUserRole(c *wkhttp.Context, bind bool) {
	if !a.requireSuperAdmin(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	if err := rejectUnexpectedBody(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	if err := a.svc.ChangeUserRole(c.Param("uid"), c.Param("role_key"), bind); err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.ResponseOK()
}

func (a *API) effectivePermissions(c *wkhttp.Context) {
	if !a.requireManager(c) {
		return
	}
	if err := rejectResourceSelectors(c); err != nil {
		respondRBACFailure(c, err)
		return
	}
	result, err := a.svc.EffectivePermissions(c.Param("uid"))
	if err != nil {
		respondRBACFailure(c, err)
		return
	}
	c.Response(result)
}

func rejectResourceSelectors(c *wkhttp.Context) error {
	for _, key := range []string{
		"group_no", "space_id", "robot_id", "resource_id", "resource", "scope",
		"member_uid", "app_id", "bot_id",
	} {
		if c.Query(key) != "" {
			return ErrInvalidScope
		}
	}
	return nil
}

func rejectUnexpectedBody(c *wkhttp.Context) error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return ErrInvalidRequest
	}
	if strings.TrimSpace(string(body)) != "" {
		return ErrInvalidRequest
	}
	return nil
}

func bindStrictJSON(c *wkhttp.Context, target interface{}) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrInvalidRequest
		}
		return err
	}
	return nil
}

func respondRBACFailure(c *wkhttp.Context, err error) {
	switch {
	case errors.Is(err, ErrRoleNotFound), errors.Is(err, ErrUserNotFound), errors.Is(err, ErrNotFound):
		respondRBACError(c, errcode.ErrSharedNotFound)
	case errors.Is(err, ErrInvalidRoleKey), errors.Is(err, ErrInvalidPermission), errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrRoleDisabled), errors.Is(err, ErrAlreadyExists):
		respondRBACError(c, errcode.ErrSharedParamInvalid)
	default:
		respondRBACError(c, errcode.ErrSharedInternal)
	}
}

func respondRBACError(c *wkhttp.Context, code codes.Code) {
	httperr.ResponseErrorLWithStatus(c, code, nil, nil)
}
