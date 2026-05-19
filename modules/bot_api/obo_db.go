// Package bot_api · YUJ-1166 / Mininglamp-OSS/octo-server#81 — Persona Clone
// (On-Behalf-Of) v0 data layer.
//
// Backing tables: obo_grants, obo_scopes (see SQL migration
// 20260519000001_obo_v0.sql). Public surface is the oboStore interface so
// HTTP handlers, checkOBO, and the fan-out listener can all be unit-tested
// against an in-memory fake without sqlmock plumbing.
//
// Cache strategy (RFC §11 risk row): the hot path on every inbound message
// asks "does grantor X have ANY active grant?". A 30-second-TTL Redis key
// `obo:grantor:{uid}` caches that scalar answer. Writes invalidate the key
// inline; ops that don't change activity state skip the invalidation. The
// fan-out path tolerates stale "true" answers (DB is still consulted for the
// scope row), and stale "false" answers cap at 30s, which is acceptable for
// v0 (see RFC §11 — risk explicitly accepted).
package bot_api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gocraft/dbr/v2"
)

// ==================== Models ====================

// oboGrantModel mirrors the obo_grants row. JSON tags are reused by HTTP
// handlers, which return rows verbatim (v0 has no nuanced DTOs).
type oboGrantModel struct {
	ID             int64      `db:"id" json:"id"`
	GrantorUID     string     `db:"grantor_uid" json:"grantor_uid"`
	GranteeBotUID  string     `db:"grantee_bot_uid" json:"grantee_bot_uid"`
	Mode           string     `db:"mode" json:"mode"`
	GlobalEnabled  int        `db:"global_enabled" json:"global_enabled"`
	Active         int        `db:"active" json:"active"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
	RevokedAt      *time.Time `db:"revoked_at" json:"revoked_at,omitempty"`
}

// oboScopeModel mirrors obo_scopes.
type oboScopeModel struct {
	ID          int64     `db:"id" json:"id"`
	GrantID     int64     `db:"grant_id" json:"grant_id"`
	ChannelID   string    `db:"channel_id" json:"channel_id"`
	ChannelType uint8     `db:"channel_type" json:"channel_type"`
	Enabled     int       `db:"enabled" json:"enabled"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

// ==================== Store interface (test seam) ====================

// oboStore is the minimal data dependency consumed by checkOBO, the REST
// handlers, and the fan-out listener. Both the production DB-backed impl and
// the test fake satisfy this surface; *botAPIDB satisfies it implicitly.
//
// Method contracts:
//   - findActiveGrantByGrantorBot: returns (nil, nil) if no row matches OR
//     the row is soft-deleted / globally disabled; callers MUST treat that as
//     "not authorized". Returning ErrNotFound was rejected because callers
//     would have to import dbr and branch on it.
//   - scopeEnabled: returns false (no error) when the scope row is missing,
//     enabled=0, or the grant_id doesn't exist. The hot path on sendMessage
//     only needs a boolean.
//   - findActiveGrantsForChannel: feeder for the fan-out listener; returns
//     active+global_enabled grants whose scope row matches the channel and
//     enabled=1. Empty slice (not nil) on no match keeps callers branch-free.
type oboStore interface {
	findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error)
	scopeEnabled(grantID int64, channelID string, channelType uint8) (bool, error)
	findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error)

	// CRUD used by the REST layer
	insertGrant(grantorUID, granteeBotUID, mode string) (int64, error)
	listGrantsByGrantor(grantorUID string) ([]*oboGrantModel, error)
	findGrantByID(id int64) (*oboGrantModel, error)
	updateGrant(id int64, mode string, globalEnabled *int) error
	revokeGrant(id int64) error
	insertScope(grantID int64, channelID string, channelType uint8, enabled int) (int64, error)
	deleteScope(id int64) error
	listScopesByGrant(grantID int64) ([]*oboScopeModel, error)
}

// Compile-time guard.
var _ oboStore = (*botAPIDB)(nil)

// ==================== Production impl (botAPIDB) ====================

const (
	// oboGrantorActiveCacheKeyFmt is the Redis key for "does grantor X have
	// at least one active grant". Lookups in the fan-out hot path consult
	// this scalar before touching MySQL. Writes that affect activity state
	// invalidate by deletion so the next reader repopulates.
	oboGrantorActiveCacheKeyFmt = "obo:grantor:%s"
	// oboCacheTTL is 30s per RFC §11. Tradeoff documented in the package
	// comment above.
	oboCacheTTL = 30 * time.Second
)

// findActiveGrantByGrantorBot — see oboStore for the contract.
func (d *botAPIDB) findActiveGrantByGrantorBot(grantorUID, granteeBotUID string) (*oboGrantModel, error) {
	if grantorUID == "" || granteeBotUID == "" {
		return nil, nil
	}
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").
		Where("grantor_uid=? AND grantee_bot_uid=? AND active=1 AND global_enabled=1",
			grantorUID, granteeBotUID).
		Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	return m, nil
}

// scopeEnabled — see oboStore.
func (d *botAPIDB) scopeEnabled(grantID int64, channelID string, channelType uint8) (bool, error) {
	if grantID == 0 || channelID == "" {
		return false, nil
	}
	var count int
	err := d.session.SelectBySql(
		"SELECT COUNT(*) FROM obo_scopes WHERE grant_id=? AND channel_id=? AND channel_type=? AND enabled=1",
		grantID, channelID, channelType,
	).LoadOne(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// findActiveGrantsForChannel — see oboStore. Single JOIN so the fan-out
// hot path doesn't have to issue a per-grant scope lookup.
func (d *botAPIDB) findActiveGrantsForChannel(channelID string, channelType uint8) ([]*oboGrantModel, error) {
	if channelID == "" {
		return []*oboGrantModel{}, nil
	}
	var grants []*oboGrantModel
	_, err := d.session.SelectBySql(
		"SELECT g.* FROM obo_grants g INNER JOIN obo_scopes s ON s.grant_id=g.id "+
			"WHERE g.active=1 AND g.global_enabled=1 AND s.enabled=1 "+
			"AND s.channel_id=? AND s.channel_type=?",
		channelID, channelType,
	).Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	return grants, nil
}

// insertGrant creates a new grant row. Returns the autoincrement ID. Unique
// constraint violations (grantor+grantee already exists) surface verbatim so
// the REST layer can translate them to 409.
func (d *botAPIDB) insertGrant(grantorUID, granteeBotUID, mode string) (int64, error) {
	if mode == "" {
		mode = "auto"
	}
	res, err := d.session.InsertInto("obo_grants").
		Columns("grantor_uid", "grantee_bot_uid", "mode", "global_enabled", "active",
			"created_at", "updated_at").
		Values(grantorUID, granteeBotUID, mode, 0, 1, time.Now(), time.Now()).
		Exec()
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Defensive: brand-new grant starts with global_enabled=0, so it cannot
	// influence the fan-out hot path until a PUT toggles it on. We still bust
	// the cache so a previously-cached "false" for this grantor is dropped.
	d.invalidateGrantorCache(grantorUID)
	return id, nil
}

// listGrantsByGrantor returns ALL rows (active + revoked) so the UI can
// surface history. Callers that only want active rows must filter.
func (d *botAPIDB) listGrantsByGrantor(grantorUID string) ([]*oboGrantModel, error) {
	var grants []*oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").
		Where("grantor_uid=?", grantorUID).
		OrderBy("created_at DESC").
		Load(&grants)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if grants == nil {
		grants = []*oboGrantModel{}
	}
	return grants, nil
}

// findGrantByID — used by the per-grant PUT/DELETE/scopes endpoints to
// resolve+authorize the row before mutating.
func (d *botAPIDB) findGrantByID(id int64) (*oboGrantModel, error) {
	var m *oboGrantModel
	_, err := d.session.Select("*").From("obo_grants").Where("id=?", id).Load(&m)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	return m, nil
}

// updateGrant applies optional fields. mode="" leaves mode untouched;
// globalEnabled=nil leaves the toggle untouched. The cache for the row's
// grantor is always invalidated because either change can flip the
// "any active grant" answer.
func (d *botAPIDB) updateGrant(id int64, mode string, globalEnabled *int) error {
	updates := map[string]interface{}{}
	if mode != "" {
		updates["mode"] = mode
	}
	if globalEnabled != nil {
		// Normalize to 0/1; anything truthy becomes 1.
		v := 0
		if *globalEnabled != 0 {
			v = 1
		}
		updates["global_enabled"] = v
	}
	if len(updates) == 0 {
		return nil
	}
	updates["updated_at"] = time.Now()
	_, err := d.session.Update("obo_grants").SetMap(updates).Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	// Cache may be wrong now; force re-read on next access.
	g, _ := d.findGrantByID(id)
	if g != nil {
		d.invalidateGrantorCache(g.GrantorUID)
	}
	return nil
}

// revokeGrant soft-deletes (active=0, global_enabled=0, revoked_at=now).
// We intentionally keep the row for audit. The FK on obo_scopes is
// ON DELETE CASCADE, which doesn't fire here — scopes remain so reactivation
// could be implemented in v1 without losing the channel list.
func (d *botAPIDB) revokeGrant(id int64) error {
	now := time.Now()
	g, err := d.findGrantByID(id)
	if err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	_, err = d.session.Update("obo_grants").SetMap(map[string]interface{}{
		"active":         0,
		"global_enabled": 0,
		"revoked_at":     now,
		"updated_at":     now,
	}).Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	d.invalidateGrantorCache(g.GrantorUID)
	return nil
}

// insertScope creates a per-channel toggle row. Duplicate (grant_id,
// channel_id, channel_type) returns the unique-key error verbatim so REST
// can translate to 409.
func (d *botAPIDB) insertScope(grantID int64, channelID string, channelType uint8, enabled int) (int64, error) {
	v := 0
	if enabled != 0 {
		v = 1
	}
	res, err := d.session.InsertInto("obo_scopes").
		Columns("grant_id", "channel_id", "channel_type", "enabled", "created_at").
		Values(grantID, channelID, channelType, v, time.Now()).
		Exec()
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Adding a new scope can extend fan-out reach; if grant is enabled the
	// per-channel hot path uses obo_scopes directly, but invalidating cache
	// keeps the contract simple.
	if g, _ := d.findGrantByID(grantID); g != nil {
		d.invalidateGrantorCache(g.GrantorUID)
	}
	return id, nil
}

// deleteScope removes a per-channel row (hard delete — there's nothing to
// audit about which channels you stopped using).
func (d *botAPIDB) deleteScope(id int64) error {
	// Look up parent grant to bust cache, then delete.
	var grantID int64
	_ = d.session.SelectBySql("SELECT grant_id FROM obo_scopes WHERE id=?", id).LoadOne(&grantID)
	_, err := d.session.DeleteFrom("obo_scopes").Where("id=?", id).Exec()
	if err != nil {
		return err
	}
	if grantID != 0 {
		if g, _ := d.findGrantByID(grantID); g != nil {
			d.invalidateGrantorCache(g.GrantorUID)
		}
	}
	return nil
}

// listScopesByGrant — REST `/v1/obo/grants/:id/scopes`.
func (d *botAPIDB) listScopesByGrant(grantID int64) ([]*oboScopeModel, error) {
	var scopes []*oboScopeModel
	_, err := d.session.Select("*").From("obo_scopes").
		Where("grant_id=?", grantID).
		OrderBy("created_at DESC").
		Load(&scopes)
	if err != nil && !errors.Is(err, dbr.ErrNotFound) {
		return nil, err
	}
	if scopes == nil {
		scopes = []*oboScopeModel{}
	}
	return scopes, nil
}

// ==================== Cache helpers ====================

// oboGrantorCacheKey returns the Redis key for the "any active grant for
// grantor" scalar. Exposed as a function (not a const) so tests can derive
// the same key without re-implementing the format string.
func oboGrantorCacheKey(grantorUID string) string {
	return fmt.Sprintf(oboGrantorActiveCacheKeyFmt, grantorUID)
}

// invalidateGrantorCache best-effort drops the cache key. Cache misses are
// safe (the hot path falls back to DB), so the cache layer cannot be a
// correctness regression and we swallow Redis errors. nil ctx is also
// tolerated for unit tests that wire *botAPIDB without Redis.
func (d *botAPIDB) invalidateGrantorCache(grantorUID string) {
	if d.ctx == nil || grantorUID == "" {
		return
	}
	redis := d.ctx.GetRedisConn()
	if redis == nil {
		return
	}
	_ = redis.Del(oboGrantorCacheKey(grantorUID))
}

// ==================== Helpers ====================

// isDuplicateKeyErr reports whether the given DB error came from a UNIQUE
// constraint violation. Used by REST handlers to translate insert errors
// into 409 Conflict without leaking driver text into the response.
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Error 1062")
}
