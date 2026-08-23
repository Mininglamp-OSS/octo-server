package adminrbac

import "errors"

var (
	ErrInvalidRoleKey    = errors.New("admin rbac: invalid role key")
	ErrInvalidScope      = errors.New("admin rbac: resource scope is not supported")
	ErrInvalidPermission = errors.New("admin rbac: unknown permission")
	ErrRoleNotFound      = errors.New("admin rbac: role not found")
	ErrRoleDisabled      = errors.New("admin rbac: role is disabled")
	ErrUserNotFound      = errors.New("admin rbac: user not found")
	ErrAlreadyExists     = errors.New("admin rbac: already exists")
	ErrNotFound          = errors.New("admin rbac: binding not found")
	ErrCacheInvalidation = errors.New("admin rbac: cache invalidation failed")
	ErrInvalidRequest    = errors.New("admin rbac: invalid request")
	ErrBootstrapConflict = errors.New("admin rbac: workplace bootstrap conflict")
)
