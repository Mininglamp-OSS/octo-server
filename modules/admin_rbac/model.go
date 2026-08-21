package adminrbac

import (
	"database/sql"
	"time"
)

const (
	activeStatus = 1

	// EffectiveCacheTTL intentionally bounds stale data even if a cache delete
	// fails. Version comparison remains the primary stale-entry guard.
	EffectiveCacheTTL = 60 * time.Second
)

type Role struct {
	ID                   int64     `db:"id" json:"id"`
	RoleKey              string    `db:"role_key" json:"role_key"`
	Name                 string    `db:"name" json:"name"`
	Description          string    `db:"description" json:"description"`
	Status               int       `db:"status" json:"status"`
	AuthorizationVersion int64     `db:"authorization_version" json:"authorization_version"`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

type RoleVersion struct {
	RoleKey              string `db:"role_key" json:"role_key"`
	AuthorizationVersion int64  `db:"authorization_version" json:"authorization_version"`
}

type RoleSnapshot struct {
	RoleKey              string
	AuthorizationVersion int64
	Status               int
	Permissions          []string
}

type EffectivePermissions struct {
	UID          string        `json:"uid"`
	Permissions  []string      `json:"permissions"`
	RoleVersions []RoleVersion `json:"role_versions"`
}

type UserRole struct {
	UID        string    `db:"uid" json:"uid"`
	RoleKey    string    `db:"role_key" json:"role_key"`
	RoleName   string    `db:"role_name" json:"role_name"`
	RoleStatus int       `db:"role_status" json:"role_status"`
	Status     int       `db:"status" json:"status"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type rolePermissionRow struct {
	RoleID               int64          `db:"role_id"`
	RoleKey              string         `db:"role_key"`
	RoleStatus           int            `db:"role_status"`
	AuthorizationVersion int64          `db:"authorization_version"`
	PermissionKey        sql.NullString `db:"permission_key"`
}
