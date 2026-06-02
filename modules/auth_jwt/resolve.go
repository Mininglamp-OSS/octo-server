package auth_jwt

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
)

// resolveSession verifies a web session token and returns (uid, spaceID).
// spaceID is the caller-supplied hint validated against space_member
// before being trusted further.
//
// Security fix (PR-D.1 #1): historically this trusted the caller-supplied
// spaceHint blindly, with a comment claiming "downstream middleware
// re-checks space_member as needed". In practice plan AU3 has downstream
// (fleet/matter) trust JWT.space_id directly — neither side actually
// checked. Result: a logged-in user could request a JWT for any space_id
// and gain cross-space read/write. Enforce membership here so AU3 holds.
//
// Delegates to pkg/auth.Decode for envelope parsing (handles v2 JSON +
// legacy uid@name fallback).
func (a *AuthJWT) resolveSession(sessionToken, spaceHint string) (string, string, error) {
	tokenPrefix := a.ctx.GetConfig().Cache.TokenCachePrefix
	raw, err := a.ctx.Cache().Get(tokenPrefix + sessionToken)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "", "", errors.New("session not found")
	}
	info, err := auth.Decode(raw)
	if err != nil {
		return "", "", err
	}
	// Empty spaceHint = caller didn't ask for a specific space; let
	// IssueWebToken decide (today it just embeds whatever we return).
	// Only validate when caller supplied one.
	if strings.TrimSpace(spaceHint) != "" {
		if err := a.assertSpaceMember(info.UID, spaceHint); err != nil {
			return "", "", err
		}
	}
	return info.UID, spaceHint, nil
}

// assertSpaceMember returns nil iff uid is an active member of spaceID.
// Pulled out so both resolveSession and bot mint can share it; mirrors
// the existing membership check inside resolveAPIKey.
func (a *AuthJWT) assertSpaceMember(uid, spaceID string) error {
	if uid == "" || spaceID == "" {
		return errors.New("assertSpaceMember: uid and space_id required")
	}
	var n int
	if err := a.ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1",
		spaceID, uid,
	).LoadOne(&n); err != nil {
		return fmt.Errorf("assertSpaceMember: %w", err)
	}
	if n == 0 {
		return errors.New("not a member of requested space")
	}
	return nil
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
	if err := a.assertSpaceMember(r.UID, r.SpaceID); err != nil {
		// Same SQL as before, just deduped via helper. Error message
		// stays specific so logs still distinguish "owner left space".
		return "", "", "", errors.New("api_key owner no longer in space")
	}
	return r.UID, r.SpaceID, daemonHint, nil
}
