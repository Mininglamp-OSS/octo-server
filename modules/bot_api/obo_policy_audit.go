package bot_api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gocraft/dbr/v2"
)

// pauseGrantAtomic preserves the legacy paused-vs-revoked distinction while
// versioning and auditing the state change. It takes the grantor lock first,
// matching create/reactivate and the activation path.
func (d *botAPIDB) pauseGrantAtomic(id int64) (*oboGrantModel, error) {
	pre, err := d.findGrantByID(id)
	if err != nil || pre == nil {
		return nil, err
	}
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	var lockHit int
	if err := tx.SelectBySql("SELECT 1 FROM `user` WHERE uid=? FOR UPDATE", pre.GrantorUID).LoadOne(&lockHit); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	var grant *oboGrantModel
	if _, err := tx.Select(oboGrantColumns).From("obo_grants").Where("id=?", id).Suffix("FOR UPDATE").Load(&grant); err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grant == nil || grant.GrantorUID != pre.GrantorUID || grant.RevokedAt != nil || grant.Active == 0 {
		return nil, nil
	}
	previous := *grant
	now := time.Now()
	if _, err := tx.Update("obo_grants").SetMap(map[string]interface{}{
		"active": 0, "updated_at": now,
	}).Where("id=? AND revoked_at IS NULL", id).Exec(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE obo_grants SET policy_version=policy_version+1 WHERE id=?", id); err != nil {
		return nil, err
	}
	grant.Active = 0
	grant.UpdatedAt = now
	if err := appendGrantPolicyAudit(tx, id, grant.GrantorUID, "pause_grant", previous, grant); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return grant, nil
}

// appendGrantPolicyAudit must be called inside the same transaction as its
// policy mutation. An audit failure aborts the mutation instead of leaving a
// successfully changed Grant with no durable record of the change.
func appendGrantPolicyAudit(tx *dbr.Tx, grantID int64, actorUID, operation string, before, after any) error {
	var beforeJSON []byte
	var err error
	if before != nil {
		beforeJSON, err = json.Marshal(before)
		if err != nil {
			return fmt.Errorf("encode previous Grant state: %w", err)
		}
	}
	var beforeValue any
	if before != nil {
		beforeValue = string(beforeJSON)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("encode current Grant state: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT INTO obo_policy_audits (grant_id,actor_uid,operation,previous_json,current_json) VALUES (?,?,?,?,?)",
		grantID, actorUID, operation, beforeValue, string(afterJSON)); err != nil {
		return fmt.Errorf("record Grant policy audit: %w", err)
	}
	return nil
}
