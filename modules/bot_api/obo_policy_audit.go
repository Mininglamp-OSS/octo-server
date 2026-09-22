package bot_api

import (
	"database/sql"
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
	var committed struct {
		PolicyVersion int64        `db:"policy_version"`
		ExpiresAt     sql.NullTime `db:"expires_at"`
	}
	if err := tx.SelectBySql("SELECT policy_version,expires_at FROM obo_grants WHERE id=?", grantID).LoadOne(&committed); err != nil {
		return fmt.Errorf("read committed Grant policy: %w", err)
	}
	var beforeValue any
	if before != nil {
		beforeJSON, err := encodeGrantAuditState(before, grantID, committed.PolicyVersion-1, committed.ExpiresAt)
		if err != nil {
			return fmt.Errorf("encode previous Grant state: %w", err)
		}
		beforeValue = string(beforeJSON)
	}
	afterJSON, err := encodeGrantAuditState(after, grantID, committed.PolicyVersion, committed.ExpiresAt)
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

func encodeGrantAuditState(value any, grantID, version int64, expiresAt sql.NullTime) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, err
	}
	state["id"] = grantID
	state["policy_version"] = version
	if expiresAt.Valid {
		state["expires_at"] = expiresAt.Time.UTC()
	} else {
		state["expires_at"] = nil
	}
	return json.Marshal(state)
}
