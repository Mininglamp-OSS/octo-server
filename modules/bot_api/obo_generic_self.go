package bot_api

import (
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/obo"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

type genericGrantStatus struct {
	GrantorUID    string     `json:"grantor_uid"`
	GrantState    string     `json:"grant_state"`
	GlobalEnabled bool       `json:"global_enabled"`
	Scopes        []string   `json:"scopes"`
	PolicyVersion int64      `json:"policy_version"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// oboBotGetGenericGrant is advisory and read-only. It never chooses an
// arbitrary Grant by Bot UID: the current owner is read first, then the exact
// (owner, Bot) pair is queried. Resolve remains the authority for decisions.
func (ba *BotAPI) oboBotGetGenericGrant(c *wkhttp.Context) {
	if getBotKindFromContext(c) != BotKindUser {
		respondBotAPIAuthFailed(c)
		return
	}
	spaceID := strings.TrimSpace(c.Query("space_id"))
	if spaceID == "" || spaceID != c.Query("space_id") {
		respondBotAPIRequestInvalid(c, "space_id")
		return
	}
	resolver := obo.Resolver{Reader: obo.DBSnapshotReader{Session: ba.db.session}}
	principal, err := resolver.Resolve(c.Request.Context(), obo.Request{
		BotToken: extractBotToken(c), Mode: obo.ModeAsBot, SpaceID: spaceID, Local: true,
	})
	if err != nil {
		ba.Warn("generic OBO status identity refused", zap.String("decision_code", obo.DecisionCode(err)))
		respondBotAPIAuthFailed(c)
		return
	}
	var owner string
	err = ba.db.session.SelectBySql("SELECT COALESCE(creator_uid,'') FROM robot WHERE robot_id=? AND status=1", principal.Actor.UID).LoadOne(&owner)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		ba.Error("generic OBO status owner lookup failed", zap.Error(err))
		respondBotAPIAuthCheckFailed(c)
		return
	}
	if owner == "" {
		c.Response([]genericGrantStatus{})
		return
	}
	var grant struct {
		ID            int64      `db:"id"`
		Active        int        `db:"active"`
		GlobalEnabled int        `db:"global_enabled"`
		RevokedAt     *time.Time `db:"revoked_at"`
		ExpiresAt     *time.Time `db:"expires_at"`
		PolicyVersion int64      `db:"policy_version"`
	}
	err = ba.db.session.SelectBySql(
		"SELECT id,active,global_enabled,revoked_at,expires_at,policy_version FROM obo_grants WHERE grantor_uid=? AND grantee_bot_uid=?",
		owner, principal.Actor.UID,
	).LoadOne(&grant)
	if errors.Is(err, dbr.ErrNotFound) {
		c.Response([]genericGrantStatus{})
		return
	}
	if err != nil {
		ba.Error("generic OBO status Grant lookup failed", zap.Error(err))
		respondBotAPIAuthCheckFailed(c)
		return
	}
	grant.RevokedAt = oboUTCTimePtrFromColumn(grant.RevokedAt)
	grant.ExpiresAt = oboUTCTimePtrFromColumn(grant.ExpiresAt)
	var all int
	if err := ba.db.session.SelectBySql("SELECT COUNT(*) FROM obo_grant_scope_bindings WHERE grant_id=? AND scope_code='ALL'", grant.ID).LoadOne(&all); err != nil {
		ba.Error("generic OBO status Scope lookup failed", zap.Error(err))
		respondBotAPIAuthCheckFailed(c)
		return
	}
	state := "ACTIVE"
	if grant.RevokedAt != nil {
		state = "REVOKED"
	} else if grant.Active != 1 {
		state = "PAUSED"
	} else if grant.ExpiresAt != nil && !grant.ExpiresAt.After(time.Now()) {
		state = "EXPIRED"
	}
	status := genericGrantStatus{GrantorUID: owner, GrantState: state,
		GlobalEnabled: grant.GlobalEnabled == 1, Scopes: []string{}, PolicyVersion: grant.PolicyVersion,
		ExpiresAt: grant.ExpiresAt}
	if all == 1 {
		status.Scopes = []string{"ALL"}
	}
	c.Response([]genericGrantStatus{status})
}
