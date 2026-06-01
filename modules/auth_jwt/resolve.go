package auth_jwt

import (
	"errors"
	"strings"
)

// resolveSession verifies a web session token and returns (uid, spaceID).
// spaceID falls back to the caller-supplied hint when the session itself
// has no canonical space (server-side sessions never store space_id today).
//
// Membership in the requested space is NOT validated here — JWT clients
// can request any space they own; downstream middleware (runtime/bot
// endpoints) re-checks space_member as needed.
func (a *AuthJWT) resolveSession(sessionToken, spaceHint string) (string, string, error) {
	tokenPrefix := a.ctx.GetConfig().Cache.TokenCachePrefix
	uidAndName, err := a.ctx.Cache().Get(tokenPrefix + sessionToken)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(uidAndName) == "" {
		return "", "", errors.New("session not found")
	}
	parts := strings.Split(uidAndName, "@")
	if len(parts) < 2 {
		return "", "", errors.New("malformed session value")
	}
	return parts[0], spaceHint, nil
}

// resolveAPIKey looks up the user_api_key row, asserts membership, and
// returns (uid, spaceID, daemonID). daemonID echoes the caller hint if
// supplied — server doesn't bind api_key→daemon_id by itself.
func (a *AuthJWT) resolveAPIKey(apiKey, daemonHint, _ string) (string, string, string, error) {
	type row struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
	}
	var r row
	_, err := a.ctx.DB().Select("uid", "space_id").From("user_api_key").
		Where("api_key=?", apiKey).Load(&r)
	if err != nil {
		return "", "", "", err
	}
	if r.UID == "" {
		return "", "", "", errors.New("invalid api_key")
	}

	var n int
	if err := a.ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		r.SpaceID, r.UID,
	).LoadOne(&n); err != nil {
		return "", "", "", err
	}
	if n == 0 {
		return "", "", "", errors.New("api_key owner no longer in space")
	}
	return r.UID, r.SpaceID, daemonHint, nil
}
