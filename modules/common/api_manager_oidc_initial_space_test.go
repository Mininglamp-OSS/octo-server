package common

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureSpaceFixtureTable creates a minimal `space` table for this package.
//
// modules/common does not import modules/space (the dependency runs the other
// way), so the space migrations are not linked into this test binary and the
// table the validator queries does not otherwise exist — same reason the space
// package hand-builds `group` / `robot` / `user` in its TestMain. Only the two
// columns the validator reads are needed; widening it would just invite drift
// from the real schema.
func ensureSpaceFixtureTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().Exec("CREATE TABLE IF NOT EXISTS `space` (" +
		"id BIGINT AUTO_INCREMENT PRIMARY KEY, " +
		"space_id VARCHAR(40) NOT NULL DEFAULT '', " +
		"name VARCHAR(100) NOT NULL DEFAULT '', " +
		"status SMALLINT NOT NULL DEFAULT 1, " +
		"created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, " +
		"updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP, " +
		"UNIQUE KEY uk_space_fixture_space_id (space_id)) " +
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci")
	require.NoError(t, err)
	_, err = ctx.DB().Exec("DELETE FROM `space`")
	require.NoError(t, err)
}

func seedFixtureSpace(t *testing.T, ctx *config.Context, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO `space` (space_id, name, status) VALUES (?, ?, ?)", spaceID, "fixture", status)
	require.NoError(t, err)
}

func readStoredSetting(t *testing.T, ctx *config.Context, category, key string) (string, bool) {
	t.Helper()
	var values []string
	_, err := ctx.DB().SelectBySql(
		"SELECT value FROM system_setting WHERE category=? AND key_name=?", category, key).Load(&values)
	require.NoError(t, err)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// TestManagerSystemSetting_OIDCInitialSpaceAcceptsActiveSpace covers acceptance
// 13: a valid target is stored and surfaces as the effective value, which is what
// the admin console reads back to confirm the setting took.
func TestManagerSystemSetting_OIDCInitialSpaceAcceptsActiveSpace(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	ensureSpaceFixtureTable(t, ctx)
	seedFixtureSpace(t, ctx, "sp-active", 1)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"space","key":"oidc_initial_space_id","value":"sp-active"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	stored, ok := readStoredSetting(t, ctx, "space", "oidc_initial_space_id")
	assert.True(t, ok, "an accepted write must persist a row")
	assert.Equal(t, "sp-active", stored)

	require.NoError(t, EnsureSystemSettings(ctx).Reload())
	assert.Equal(t, "sp-active", EnsureSystemSettings(ctx).OIDCInitialSpaceID())
}

// TestManagerSystemSetting_OIDCInitialSpaceRejectsMissingSpace covers acceptance
// 12 for an id that was never valid, and pins that nothing is persisted: a
// rejected write that still stored the value would leave the feature pointing at
// a Space that does not exist, with no error anywhere afterwards.
func TestManagerSystemSetting_OIDCInitialSpaceRejectsMissingSpace(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	ensureSpaceFixtureTable(t, ctx)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"space","key":"oidc_initial_space_id","value":"sp-nope"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.Equal(t, "err.server.common.oidc_initial_space_invalid", body.Error.Code)
	assert.Equal(t, "oidc_initial_space_id", body.Error.Details["field"],
		"the admin console points at the offending field with this")

	_, ok := readStoredSetting(t, ctx, "space", "oidc_initial_space_id")
	assert.False(t, ok, "a rejected write must persist nothing")
}

// TestManagerSystemSetting_OIDCInitialSpaceRejectsInactiveSpace pins that a
// Space that exists but is disbanded or banned is refused too. Checking only for
// existence would accept a dead Space and produce exactly the stranded-user
// symptom this feature exists to fix, while looking configured.
func TestManagerSystemSetting_OIDCInitialSpaceRejectsInactiveSpace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"disbanded", 0},
		{"banned", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, ctx := newSuperAdminServer(t)
			ensureSpaceFixtureTable(t, ctx)
			seedFixtureSpace(t, ctx, "sp-dead", tc.status)

			w := postSystemSetting(t, route, `{"items":[
				{"category":"space","key":"oidc_initial_space_id","value":"sp-dead"}
			]}`)
			require.NotEqual(t, http.StatusOK, w.Code, w.Body.String())

			_, ok := readStoredSetting(t, ctx, "space", "oidc_initial_space_id")
			assert.False(t, ok, "a rejected write must persist nothing")
		})
	}
}

// TestManagerSystemSetting_OIDCInitialSpaceEmptyValueTurnsFeatureOff covers the
// off switch half of acceptance 13. Empty must skip the existence check
// entirely — validating it would make the feature impossible to turn off once
// the Space it pointed at was disbanded, which is precisely when an operator
// most wants to.
func TestManagerSystemSetting_OIDCInitialSpaceEmptyValueTurnsFeatureOff(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	ensureSpaceFixtureTable(t, ctx)
	seedFixtureSpace(t, ctx, "sp-active", 1)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"space","key":"oidc_initial_space_id","value":"sp-active"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The Space goes away underneath the configuration, then the operator clears
	// the setting. No table lookup may stand in the way.
	_, err := ctx.DB().Exec("UPDATE `space` SET status=0 WHERE space_id=?", "sp-active")
	require.NoError(t, err)

	w = postSystemSetting(t, route, `{"items":[
		{"category":"space","key":"oidc_initial_space_id","value":""}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.NoError(t, EnsureSystemSettings(ctx).Reload())
	assert.Empty(t, EnsureSystemSettings(ctx).OIDCInitialSpaceID(), "feature must be off")
}

// TestManagerSystemSetting_OIDCInitialSpaceTrimsStoredValue pins that the value
// is trimmed on the way in, not merely on the way out.
//
// A space_id pasted from the console routinely carries whitespace. Validating a
// trimmed copy while storing the raw one would accept the write, show the padded
// value as `value` and the trimmed one as `effective_value` in the same GET
// response, and leave a future reader that does not trim looking up a Space that
// does not match.
func TestManagerSystemSetting_OIDCInitialSpaceTrimsStoredValue(t *testing.T) {
	route, ctx := newSuperAdminServer(t)
	ensureSpaceFixtureTable(t, ctx)
	seedFixtureSpace(t, ctx, "sp-active", 1)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"space","key":"oidc_initial_space_id","value":"  sp-active  "}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	stored, ok := readStoredSetting(t, ctx, "space", "oidc_initial_space_id")
	require.True(t, ok)
	assert.Equal(t, "sp-active", stored, "the stored value must already be trimmed")
}

// TestManagerSystemSetting_OIDCInitialSpaceIgnoresUnrelatedBatches pins that a
// batch not touching this key pays nothing for the check — no Space lookup, and
// no rejection. Without the fixture table present, a validator that ran
// unconditionally would fail the query and turn every unrelated settings write
// into a 500.
func TestManagerSystemSetting_OIDCInitialSpaceIgnoresUnrelatedBatches(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"register","key":"email_on","value":"1"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
