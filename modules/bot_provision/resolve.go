package bot_provision

import (
	"errors"
	"fmt"
)

// assertSpaceMember returns nil iff uid is an active member of an active
// (non-disabled) space. v3 §2.3 (Jerry-Xin Critical 1): joining `space` for
// status=1 closes the case where a soft-deleted space still has lingering
// active space_member rows — without it, an api_key bound to a disabled
// space would keep validating. Mirrors modules/space/db.go canonical
// (s.status=1 + sm.status=1) pattern.
// Used by both mintBot (web caller) and resolveAPIKey (daemon caller).
func (a *BotProvision) assertSpaceMember(uid, spaceID string) error {
	if uid == "" || spaceID == "" {
		return errors.New("assertSpaceMember: uid and space_id required")
	}
	var n int
	if err := a.ctx.DB().SelectBySql(
		`SELECT COUNT(*) FROM space_member sm
		 INNER JOIN space s ON s.space_id=sm.space_id AND s.status=1
		 WHERE sm.space_id=? AND sm.uid=? AND sm.status=1`,
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
// returns (uid, spaceID).
//
// 合并 plan 决策一+二 Phase 4: 砍掉 resolveSession (JWT exchange 没了, 没人
// 调). 这里保留 resolveAPIKey 给 botToken (daemon → bot_token) 用.
//
// v3.2 cleanup: dropped the (daemonHint, _ string) params and the trailing
// daemon-id return — they were leftovers from the JWT-exchange contract
// where the server echoed daemon_hint back as the JWT.daemon_id claim.
// With Phase 4 sending api_key directly there's no JWT to claim into, so
// daemon_id never needed to round-trip through resolveAPIKey. Callers
// already passed empty strings (bot_api.go:111: `resolveAPIKey(apiKey,
// "", "")`) and discarded the 3rd return.
func (a *BotProvision) resolveAPIKey(apiKey string) (string, string, error) {
	type row struct {
		UID     string `db:"uid"`
		SpaceID string `db:"space_id"`
	}
	var r row
	_, err := a.ctx.DB().Select("uid", "space_id").From("user_api_key").
		Where("api_key=? AND space_id!=''", apiKey).Load(&r)
	if err != nil {
		return "", "", err
	}
	if r.UID == "" {
		return "", "", errors.New("invalid api_key")
	}
	if err := a.assertSpaceMember(r.UID, r.SpaceID); err != nil {
		return "", "", errors.New("api_key owner no longer in space")
	}
	return r.UID, r.SpaceID, nil
}
