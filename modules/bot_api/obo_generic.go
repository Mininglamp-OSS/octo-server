package bot_api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

var errGenericBotNotOwned = errors.New("obo: Bot is not owned by grantor")

const genericManagementMaxBodyBytes = 4096

func bindGenericManagementJSON(c *wkhttp.Context, dst any) error {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, genericManagementMaxBodyBytes+1))
	if err != nil || len(body) > genericManagementMaxBodyBytes {
		return errors.New("invalid management request body")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing management request data")
	}
	return nil
}

type putDelegationRequest struct {
	Active        *bool          `json:"active"`
	GlobalEnabled *bool          `json:"global_enabled"`
	ScopeCodes    []string       `json:"scope_codes"`
	ExpiresAt     optionalExpiry `json:"expires_at"`
}

// A value field preserves absent vs explicit null: absent leaves an existing
// deadline alone, while null removes it. Wire timestamps require an offset and
// are stored as UTC DATETIME values.
type optionalExpiry struct {
	Set   bool
	Value *time.Time
}

func (o *optionalExpiry) UnmarshalJSON(data []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Value = nil
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("expires_at must be an RFC3339 timestamp: %w", err)
	}
	utc := parsed.UTC()
	o.Value = &utc
	return nil
}

type genericDelegationView struct {
	GrantID       int64      `json:"grant_id"`
	GranteeBotUID string     `json:"grantee_bot_uid"`
	Mode          string     `json:"mode"`
	Active        bool       `json:"active"`
	GlobalEnabled bool       `json:"global_enabled"`
	ScopeCodes    []string   `json:"scope_codes"`
	PolicyVersion int64      `json:"policy_version"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

func normalizeGenericScopes(codes []string) (bool, error) {
	if codes == nil || len(codes) > 1 {
		return false, errors.New("scope_codes must be [] or [ALL]")
	}
	if len(codes) == 0 {
		return false, nil
	}
	if codes[0] != "ALL" {
		return false, errors.New("only ALL Scope is supported")
	}
	return true, nil
}

func (ba *BotAPI) oboPutDelegation(c *wkhttp.Context) {
	ownerUID := strings.TrimSpace(c.GetLoginUID())
	botUID := strings.TrimSpace(c.Param("bot_uid"))
	if ownerUID == "" || botUID == "" || ownerUID == botUID {
		respondBotAPIRequestInvalid(c, "bot_uid")
		return
	}
	var req putDelegationRequest
	if err := bindGenericManagementJSON(c, &req); err != nil || req.Active == nil || req.GlobalEnabled == nil {
		respondBotAPIRequestInvalid(c, "active|global_enabled|scope_codes")
		return
	}
	all, err := normalizeGenericScopes(req.ScopeCodes)
	if err != nil {
		respondBotAPIRequestInvalid(c, "scope_codes")
		return
	}
	view, err := ba.db.putDelegationAtomic(c.Request.Context(), ownerUID, botUID, *req.Active, *req.GlobalEnabled, all, req.ExpiresAt)
	if errors.Is(err, errGenericBotNotOwned) {
		httperr.ResponseErrorLWithStatus(c, errcode.ErrBotAPIBotNotRegistered, nil, nil)
		return
	}
	if err != nil {
		ba.Error("generic OBO delegation write failed", zap.Error(err), zap.String("owner", ownerUID), zap.String("bot", botUID))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIOBOInternal, nil, nil)
		return
	}
	c.Response(view)
}

type genericGrantRow struct {
	ID            int64      `db:"id" json:"grant_id"`
	BotUID        string     `db:"grantee_bot_uid" json:"-"`
	Mode          string     `db:"mode" json:"mode"`
	Active        int        `db:"active" json:"active"`
	GlobalEnabled int        `db:"global_enabled" json:"global_enabled"`
	RevokedAt     *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
	ExpiresAt     *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	PolicyVersion int64      `db:"policy_version" json:"policy_version"`
}

const updateGenericGrantSQL = "UPDATE obo_grants SET active=?, global_enabled=?, " +
	"persona_prompt=CASE WHEN ?=1 THEN '' ELSE persona_prompt END, " +
	"revoked_at=CASE WHEN ?=1 THEN NULL ELSE revoked_at END, " +
	"expires_at=CASE WHEN ?=1 THEN ? ELSE expires_at END, " +
	"policy_version=policy_version+1, updated_at=CURRENT_TIMESTAMP WHERE id=?"

func intBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

// putDelegationAtomic locks the Human row first, mirroring the existing
// single-active-persona lock order. Grant state, ALL binding, sibling demotion,
// and management audit commit together.
func (d *botAPIDB) putDelegationAtomic(ctx context.Context, ownerUID, botUID string, active, globalEnabled, all bool, expiry optionalExpiry) (*genericDelegationView, error) {
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
	var currentOwner string
	err = tx.SelectBySql("SELECT COALESCE(creator_uid,'') FROM robot WHERE robot_id=? AND status=1 FOR UPDATE", botUID).LoadOne(&currentOwner)
	if errors.Is(err, dbr.ErrNotFound) {
		return nil, errGenericBotNotOwned
	}
	if err != nil {
		return nil, fmt.Errorf("check Bot ownership: %w", err)
	}
	if currentOwner != ownerUID {
		return nil, errGenericBotNotOwned
	}
	var botAccount int
	if err := tx.SelectBySql("SELECT COUNT(*) FROM user WHERE uid=? AND robot=1 AND status=1 AND COALESCE(is_destroy,0)<>2", botUID).LoadOne(&botAccount); err != nil {
		return nil, fmt.Errorf("check Bot account: %w", err)
	}
	if botAccount != 1 {
		return nil, errGenericBotNotOwned
	}

	var old genericGrantRow
	err = tx.SelectBySql("SELECT id, mode, active, global_enabled, revoked_at, expires_at, policy_version FROM obo_grants WHERE grantor_uid=? AND grantee_bot_uid=? FOR UPDATE", ownerUID, botUID).LoadOne(&old)
	created := errors.Is(err, dbr.ErrNotFound)
	if err != nil && !created {
		return nil, fmt.Errorf("lookup Grant: %w", err)
	}
	if !created {
		normalizeGenericGrantRowTimestamps(&old)
	}
	if created {
		var expiresAt any
		if expiry.Value != nil {
			expiresAt = expiry.Value.UTC().Format("2006-01-02 15:04:05.999999")
		}
		result, err := tx.ExecContext(ctx,
			"INSERT INTO obo_grants (grantor_uid,grantee_bot_uid,mode,global_enabled,active,policy_version,persona_prompt,expires_at) VALUES (?,?,'auto',?,?,1,'',?)",
			ownerUID, botUID, intBool(globalEnabled), intBool(active), expiresAt)
		if err != nil {
			return nil, fmt.Errorf("insert Grant: %w", err)
		}
		old.ID, err = result.LastInsertId()
		if err != nil {
			return nil, err
		}
		old.PolicyVersion = 1
		old.Mode = "auto"
		old.Active = intBool(active)
		old.GlobalEnabled = intBool(globalEnabled)
		old.ExpiresAt = expiry.Value
	}

	var hadALL int
	if err := tx.SelectBySql("SELECT COUNT(*) FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", old.ID).LoadOne(&hadALL); err != nil {
		return nil, fmt.Errorf("lookup ALL binding: %w", err)
	}
	expiresAt := old.ExpiresAt
	expiryChanged := false
	if expiry.Set {
		expiryChanged = (old.ExpiresAt == nil) != (expiry.Value == nil) ||
			(old.ExpiresAt != nil && expiry.Value != nil && !old.ExpiresAt.Equal(*expiry.Value))
		expiresAt = expiry.Value
	}
	changed := created || old.Active != intBool(active) || old.GlobalEnabled != intBool(globalEnabled) || (active && old.RevokedAt != nil) || (hadALL == 1) != all || expiryChanged
	targetChanged := changed
	previous := genericDelegationView{
		GrantID: old.ID, GranteeBotUID: botUID,
		Mode: old.Mode, ExpiresAt: old.ExpiresAt,
		Active:        old.Active == 1 && old.RevokedAt == nil,
		GlobalEnabled: old.GlobalEnabled == 1,
		ScopeCodes:    []string{}, PolicyVersion: old.PolicyVersion,
	}
	if hadALL == 1 {
		previous.ScopeCodes = []string{"ALL"}
	}
	if !created && changed {
		var expirySQL any
		if expiry.Value != nil {
			expirySQL = expiry.Value.UTC().Format("2006-01-02 15:04:05.999999")
		}
		// An active PUT reauthorizes a previously revoked Grant. Clear the
		// legacy Channel persona so stale instructions cannot survive revival.
		reauthorize := active && old.RevokedAt != nil
		if _, err := tx.ExecContext(ctx, updateGenericGrantSQL,
			intBool(active), intBool(globalEnabled), intBool(reauthorize), intBool(reauthorize),
			intBool(expiry.Set), expirySQL, old.ID); err != nil {
			return nil, fmt.Errorf("update Grant: %w", err)
		}
		old.PolicyVersion++
	}
	if all && hadALL == 0 {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO obo_grant_scope_bindings (grant_id,scope_code,assigned_by) VALUES (?,'ALL',?)", old.ID, ownerUID); err != nil {
			return nil, fmt.Errorf("bind ALL: %w", err)
		}
	} else if !all && hadALL == 1 {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", old.ID); err != nil {
			return nil, fmt.Errorf("unbind ALL: %w", err)
		}
	}
	var siblings []*struct {
		ID            int64 `db:"id"`
		Active        int   `db:"active"`
		GlobalEnabled int   `db:"global_enabled"`
		PolicyVersion int64 `db:"policy_version"`
	}
	if active {
		if _, err := tx.SelectBySql(
			"SELECT id,active,global_enabled,policy_version FROM obo_grants "+
				"WHERE grantor_uid=? AND active=1 AND id<>? AND revoked_at IS NULL FOR UPDATE", ownerUID, old.ID,
		).Load(&siblings); err != nil && !errors.Is(err, dbr.ErrNotFound) {
			return nil, fmt.Errorf("scan active siblings: %w", err)
		}
		for _, sibling := range siblings {
			if _, err := tx.ExecContext(ctx,
				"UPDATE obo_grants SET active=0,global_enabled=0,policy_version=policy_version+1,updated_at=CURRENT_TIMESTAMP WHERE id=?",
				sibling.ID); err != nil {
				return nil, fmt.Errorf("demote sibling persona %d: %w", sibling.ID, err)
			}
			if err := appendGrantPolicyAudit(tx, sibling.ID, ownerUID, "demote_sibling",
				map[string]any{"active": sibling.Active, "global_enabled": sibling.GlobalEnabled},
				map[string]any{"active": 0, "global_enabled": 0}); err != nil {
				return nil, fmt.Errorf("audit sibling persona %d: %w", sibling.ID, err)
			}
			changed = true
		}
	}
	view := &genericDelegationView{
		GrantID: old.ID, GranteeBotUID: botUID, Active: active,
		Mode: old.Mode, ExpiresAt: expiresAt,
		GlobalEnabled: globalEnabled, ScopeCodes: []string{}, PolicyVersion: old.PolicyVersion,
	}
	if all {
		view.ScopeCodes = []string{"ALL"}
	}
	if targetChanged {
		var beforeJSON []byte
		if !created {
			beforeJSON, err = json.Marshal(previous)
			if err != nil {
				return nil, err
			}
		}
		afterJSON, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		var previousValue any
		if !created {
			previousValue = string(beforeJSON)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO obo_policy_audits (grant_id,actor_uid,operation,previous_json,current_json) VALUES (?,?,'put_delegation',?,?)",
			old.ID, ownerUID, previousValue, string(afterJSON)); err != nil {
			return nil, fmt.Errorf("audit Grant change: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit Grant change: %w", err)
	}
	if changed {
		d.invalidateGrantorCache(ownerUID)
		ids := []int64{old.ID}
		for _, sibling := range siblings {
			ids = append(ids, sibling.ID)
		}
		for _, id := range ids {
			scopes, _ := d.listScopesByGrant(id)
			for _, scope := range scopes {
				d.invalidateChannelCache(scope.ChannelID, scope.ChannelType)
			}
		}
	}
	return view, nil
}
