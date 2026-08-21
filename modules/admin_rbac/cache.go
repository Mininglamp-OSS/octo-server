package adminrbac

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/cache"
	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
)

const (
	permissionCacheNamespace = "admin_rbac"
)

type PermissionCache struct {
	cache cache.Cache
}

type cacheEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	UID           string        `json:"uid"`
	RoleVersions  []RoleVersion `json:"role_versions"`
	Permissions   []string      `json:"permissions"`
	ExpiresAt     int64         `json:"expires_at"`
}

func NewPermissionCache(c cache.Cache) *PermissionCache {
	return &PermissionCache{cache: c}
}

func (c *PermissionCache) userKey(uid string) string {
	return fmt.Sprintf("%s:effective:%d:%s", permissionCacheNamespace, authz.PermissionContractSchemaVersion, uid)
}

func (c *PermissionCache) roleKey(roleKey string) string {
	return fmt.Sprintf("%s:role:%d:%s", permissionCacheNamespace, authz.PermissionContractSchemaVersion, roleKey)
}

func (c *PermissionCache) Get(uid string, roleVersions []RoleVersion) (EffectivePermissions, bool, error) {
	if c == nil || c.cache == nil || uid == "" {
		return EffectivePermissions{}, false, nil
	}
	raw, err := c.cache.Get(c.userKey(uid))
	if err != nil || raw == "" {
		return EffectivePermissions{}, false, nil
	}
	var envelope cacheEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return EffectivePermissions{}, false, nil
	}
	if envelope.SchemaVersion != authz.PermissionContractSchemaVersion || envelope.UID != uid ||
		envelope.ExpiresAt <= time.Now().Unix() ||
		!roleVersionsEqual(envelope.RoleVersions, roleVersions) {
		return EffectivePermissions{}, false, nil
	}
	if !sort.StringsAreSorted(envelope.Permissions) {
		return EffectivePermissions{}, false, nil
	}
	for _, permissionKey := range envelope.Permissions {
		if err := validatePermissionKey(permissionKey); err != nil {
			return EffectivePermissions{}, false, nil
		}
	}
	return EffectivePermissions{UID: uid, Permissions: envelope.Permissions, RoleVersions: envelope.RoleVersions}, true, nil
}

func (c *PermissionCache) Set(result EffectivePermissions) error {
	if c == nil || c.cache == nil || result.UID == "" {
		return nil
	}
	envelope := cacheEnvelope{
		SchemaVersion: authz.PermissionContractSchemaVersion,
		UID:           result.UID,
		RoleVersions:  append([]RoleVersion(nil), result.RoleVersions...),
		Permissions:   append([]string(nil), result.Permissions...),
		ExpiresAt:     time.Now().Add(EffectiveCacheTTL).Unix(),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return c.cache.SetAndExpire(c.userKey(result.UID), string(data), EffectiveCacheTTL)
}

func (c *PermissionCache) InvalidateUser(uid string) error {
	if c == nil || c.cache == nil || uid == "" {
		return nil
	}
	return c.cache.Delete(c.userKey(uid))
}

func (c *PermissionCache) InvalidateRole(roleKey string) error {
	if c == nil || c.cache == nil || roleKey == "" {
		return nil
	}
	// User snapshots are invalidated by the role version check. The role key
	// also provides a stable namespace for a future role-level materialization.
	return c.cache.Delete(c.roleKey(roleKey))
}
