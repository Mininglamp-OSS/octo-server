package obo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/gocraft/dbr/v2"
)

// DBSnapshotReader performs every mutable identity/authorization read inside
// one repeatable-read transaction. It does not consult legacy OBO caches.
type DBSnapshotReader struct{ Session *dbr.Session }

// policyGrantMode is stored in obo_grants.mode to separate the OBO
// authorization policy from the deprecated Persona runtime without another
// table.
const policyGrantMode = "policy"

// The first predicate names robot's functional token index; the binary
// predicate keeps the final comparison byte-exact under case-insensitive
// collations.
const botTokenLookupSQL = "SELECT robot_id, COALESCE(creator_uid,'') AS creator_uid FROM robot " +
	"WHERE NULLIF(bot_token,'')=? AND BINARY bot_token=BINARY ? AND status=1 LIMIT 1"

func (r DBSnapshotReader) Read(ctx context.Context, botToken, spaceID string, mode Mode) (Snapshot, error) {
	if r.Session == nil {
		return Snapshot{}, errors.New("obo: DB session is nil")
	}
	tx, err := r.Session.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("obo: begin authorization snapshot: %w", err)
	}
	defer tx.RollbackUnlessCommitted()
	var bot struct {
		UID      string `db:"robot_id"`
		OwnerUID string `db:"creator_uid"`
	}
	err = tx.SelectBySql(botTokenLookupSQL, botToken, botToken).LoadOne(&bot)
	if errors.Is(err, dbr.ErrNotFound) {
		return Snapshot{}, deny("invalid_credential", http.StatusUnauthorized)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("obo: Bot token lookup: %w", err)
	}
	var activeBot int
	err = tx.SelectBySql(
		"SELECT COUNT(*) FROM user WHERE uid=? AND robot=1 AND status=1 AND COALESCE(is_destroy,0)<>2", bot.UID,
	).LoadOne(&activeBot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("obo: Bot account lookup: %w", err)
	}
	if activeBot != 1 {
		return Snapshot{}, deny("disabled", http.StatusForbidden)
	}
	var activeSpace int
	err = tx.SelectBySql("SELECT COUNT(*) FROM space WHERE space_id=? AND status=1", spaceID).LoadOne(&activeSpace)
	if err != nil {
		return Snapshot{}, fmt.Errorf("obo: Space lookup: %w", err)
	}
	var botSeat int
	err = tx.SelectBySql(
		"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, bot.UID,
	).LoadOne(&botSeat)
	if err != nil {
		return Snapshot{}, fmt.Errorf("obo: Bot Space membership lookup: %w", err)
	}
	if activeSpace != 1 || botSeat != 1 {
		return Snapshot{}, deny("space_not_allowed", http.StatusForbidden)
	}
	state := Snapshot{BotUID: bot.UID}
	if mode == ModeOBO {
		if bot.OwnerUID == "" {
			return Snapshot{}, deny("delegation_denied", http.StatusForbidden)
		}
		var activeOwner int
		err = tx.SelectBySql(
			"SELECT COUNT(*) FROM user WHERE uid=? AND robot=0 AND status=1 AND COALESCE(is_destroy,0)<>2", bot.OwnerUID,
		).LoadOne(&activeOwner)
		if err != nil {
			return Snapshot{}, fmt.Errorf("obo: Human owner lookup: %w", err)
		}
		if activeOwner != 1 {
			return Snapshot{}, deny("delegation_denied", http.StatusForbidden)
		}
		var ownerSeat int
		err = tx.SelectBySql(
			"SELECT COUNT(*) FROM space_member WHERE space_id=? AND uid=? AND status=1", spaceID, bot.OwnerUID,
		).LoadOne(&ownerSeat)
		if err != nil {
			return Snapshot{}, fmt.Errorf("obo: Human owner Space membership lookup: %w", err)
		}
		if ownerSeat != 1 {
			return Snapshot{}, deny("space_not_allowed", http.StatusForbidden)
		}
		state.OwnerUID = bot.OwnerUID
		var grant struct {
			ID      int64 `db:"id"`
			Version int64 `db:"policy_version"`
		}
		err = tx.SelectBySql(
			"SELECT id, policy_version FROM obo_grants WHERE grantor_uid=? AND grantee_bot_uid=? "+
				"AND mode='"+policyGrantMode+"' AND active=1 AND global_enabled=1 AND revoked_at IS NULL "+
				"AND (expires_at IS NULL OR expires_at>UTC_TIMESTAMP(6)) LIMIT 1",
			bot.OwnerUID, bot.UID,
		).LoadOne(&grant)
		if errors.Is(err, dbr.ErrNotFound) {
			return Snapshot{}, deny("delegation_denied", http.StatusForbidden)
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("obo: Grant lookup: %w", err)
		}
		state.GrantID = grant.ID
		state.PolicyVersion = grant.Version
		var bindings []struct {
			ScopeCode string `db:"scope_code"`
		}
		_, err = tx.SelectBySql(
			"SELECT scope_code FROM obo_grant_scope_bindings WHERE grant_id=?", grant.ID,
		).Load(&bindings)
		if err != nil && !errors.Is(err, dbr.ErrNotFound) {
			return Snapshot{}, fmt.Errorf("obo: Scope binding lookup: %w", err)
		}
		for _, binding := range bindings {
			state.BoundScopes = append(state.BoundScopes, binding.ScopeCode)
		}
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("obo: commit authorization read: %w", err)
	}
	return state, nil
}
