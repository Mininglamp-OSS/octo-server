package bot_api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

const genericGrantForOwnerSQL = "SELECT g.id,g.grantee_bot_uid,g.mode,g.active,g.global_enabled,g.revoked_at,g.expires_at,g.policy_version " +
	"FROM obo_grants g " +
	"INNER JOIN robot r ON r.robot_id=g.grantee_bot_uid AND r.status=1 AND r.creator_uid=? " +
	"INNER JOIN user u ON u.uid=? AND u.robot=0 AND u.status=1 AND COALESCE(u.is_destroy,0)<>2 " +
	"WHERE g.id=? AND g.grantor_uid=? AND g.grantor_uid<>''"

func (ba *BotAPI) genericGrantForOwner(ownerUID string, id int64) (*genericGrantRow, error) {
	if ownerUID == "" {
		return nil, errGenericBotNotOwned
	}
	var grant genericGrantRow
	err := ba.db.session.SelectBySql(genericGrantForOwnerSQL, ownerUID, ownerUID, id, ownerUID).LoadOne(&grant)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, errGenericBotNotOwned
	}
	if err != nil {
		return nil, err
	}
	return normalizeGenericGrantRowTimestamps(&grant), nil
}

func (ba *BotAPI) genericViewForOwner(ownerUID string, id int64) (*genericDelegationView, error) {
	grant, err := ba.genericGrantForOwner(ownerUID, id)
	if err != nil {
		return nil, err
	}
	var all int
	if err := ba.db.session.SelectBySql(
		"SELECT COUNT(*) FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", id,
	).LoadOne(&all); err != nil {
		return nil, err
	}
	view := &genericDelegationView{GrantID: id, GranteeBotUID: grant.BotUID,
		Mode: grant.Mode, ExpiresAt: grant.ExpiresAt,
		Active:        grant.Active == 1 && grant.RevokedAt == nil,
		GlobalEnabled: grant.GlobalEnabled == 1, PolicyVersion: grant.PolicyVersion,
		ScopeCodes: []string{}}
	if all == 1 {
		view.ScopeCodes = []string{"ALL"}
	}
	return view, nil
}

func (ba *BotAPI) respondGenericManagementError(c *wkhttp.Context, err error, operation string) {
	if errors.Is(err, errGenericBotNotOwned) {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIOBOGrantNotFound, nil, nil)
		return
	}
	ba.Error("generic OBO management failed", zap.Error(err), zap.String("operation", operation))
	httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
}

func (ba *BotAPI) oboGetGenericGrant(c *wkhttp.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	view, err := ba.genericViewForOwner(c.GetLoginUID(), id)
	if err != nil {
		ba.respondGenericManagementError(c, err, "get_grant")
		return
	}
	c.Response(view)
}

func (ba *BotAPI) oboListGenericBindings(c *wkhttp.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	view, err := ba.genericViewForOwner(c.GetLoginUID(), id)
	if err != nil {
		ba.respondGenericManagementError(c, err, "list_bindings")
		return
	}
	c.Response(map[string]any{"items": view.ScopeCodes, "policy_version": view.PolicyVersion})
}

func (ba *BotAPI) oboBindGenericScope(c *wkhttp.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		ScopeCode string `json:"scope_code"`
	}
	if err := bindGenericManagementJSON(c, &req); err != nil || req.ScopeCode != "ALL" {
		respondBotAPIRequestInvalid(c, "scope_code")
		return
	}
	view, err := ba.db.setGenericBinding(c.Request.Context(), c.GetLoginUID(), id, true)
	if err != nil {
		ba.respondGenericManagementError(c, err, "bind_scope")
		return
	}
	c.Response(view)
}

func (ba *BotAPI) oboUnbindGenericScope(c *wkhttp.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	view, err := ba.db.setGenericBinding(c.Request.Context(), c.GetLoginUID(), id, false)
	if err != nil {
		ba.respondGenericManagementError(c, err, "unbind_scope")
		return
	}
	c.Response(view)
}

func (d *botAPIDB) setGenericBinding(ctx context.Context, ownerUID string, id int64, bind bool) (*genericDelegationView, error) {
	tx, err := d.session.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()
	var owner string
	err = tx.SelectBySql("SELECT uid FROM user WHERE uid=? AND robot=0 AND status=1 AND COALESCE(is_destroy,0)<>2 FOR UPDATE", ownerUID).LoadOne(&owner)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, errGenericBotNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("lock grantor: %w", err)
	}
	var grant genericGrantRow
	err = tx.SelectBySql(
		"SELECT id,grantee_bot_uid,mode,active,global_enabled,revoked_at,expires_at,policy_version FROM obo_grants WHERE id=? AND grantor_uid=? FOR UPDATE",
		id, ownerUID,
	).LoadOne(&grant)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, errGenericBotNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("lock Grant: %w", err)
	}
	normalizeGenericGrantRowTimestamps(&grant)
	if grant.RevokedAt != nil {
		return nil, errGenericBotNotOwned
	}
	var currentOwner string
	err = tx.SelectBySql("SELECT COALESCE(creator_uid,'') FROM robot WHERE robot_id=? AND status=1", grant.BotUID).LoadOne(&currentOwner)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, errGenericBotNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("check current Bot owner: %w", err)
	}
	if currentOwner != ownerUID {
		return nil, errGenericBotNotOwned
	}
	var hadALL int
	if err := tx.SelectBySql("SELECT COUNT(*) FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", id).LoadOne(&hadALL); err != nil {
		return nil, fmt.Errorf("lookup binding: %w", err)
	}
	if (hadALL == 1) != bind {
		if bind {
			_, err = tx.ExecContext(ctx, "INSERT INTO obo_grant_scope_bindings (grant_id,scope_code,assigned_by) VALUES (?,'ALL',?)", id, ownerUID)
		} else {
			_, err = tx.ExecContext(ctx, "DELETE FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", id)
		}
		if err != nil {
			return nil, fmt.Errorf("update binding: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE obo_grants SET policy_version=policy_version+1,updated_at=CURRENT_TIMESTAMP WHERE id=?", id); err != nil {
			return nil, fmt.Errorf("increment policy version: %w", err)
		}
		grant.PolicyVersion++
		previousScopes := []string{}
		if hadALL == 1 {
			previousScopes = []string{"ALL"}
		}
		currentScopes := []string{}
		if bind {
			currentScopes = []string{"ALL"}
		}
		if err := appendGrantPolicyAudit(tx, id, ownerUID, "set_scope_binding",
			map[string]any{"scope_codes": previousScopes},
			map[string]any{"scope_codes": currentScopes}); err != nil {
			return nil, fmt.Errorf("audit binding: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	view := &genericDelegationView{GrantID: id, GranteeBotUID: grant.BotUID,
		Mode: grant.Mode, ExpiresAt: grant.ExpiresAt,
		Active: grant.Active == 1, GlobalEnabled: grant.GlobalEnabled == 1,
		PolicyVersion: grant.PolicyVersion, ScopeCodes: []string{}}
	if bind {
		view.ScopeCodes = []string{"ALL"}
	}
	return view, nil
}

type genericAuditRow struct {
	ID           int64     `db:"id" json:"id"`
	GrantID      int64     `db:"grant_id" json:"grant_id"`
	ActorUID     string    `db:"actor_uid" json:"actor_uid"`
	Operation    string    `db:"operation" json:"operation"`
	PreviousJSON string    `db:"previous_json" json:"previous_json"`
	CurrentJSON  string    `db:"current_json" json:"current_json"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

func (ba *BotAPI) oboListPolicyAudits(c *wkhttp.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if _, err := ba.genericGrantForOwner(c.GetLoginUID(), id); err != nil {
		ba.respondGenericManagementError(c, err, "audit_access")
		return
	}
	var rows []genericAuditRow
	_, err := ba.db.session.SelectBySql(
		"SELECT id,grant_id,actor_uid,operation,COALESCE(previous_json,'') AS previous_json,current_json,created_at "+
			"FROM obo_policy_audits WHERE grant_id=? ORDER BY id DESC LIMIT 100", id,
	).Load(&rows)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		ba.respondGenericManagementError(c, err, "audit_list")
		return
	}
	if rows == nil {
		rows = []genericAuditRow{}
	}
	for i := range rows {
		rows[i].CreatedAt = oboUTCFromColumn(rows[i].CreatedAt)
	}
	c.Response(map[string]any{"items": rows})
}
