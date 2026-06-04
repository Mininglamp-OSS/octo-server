package botfather

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

func setupAPIKeyServiceTest(t *testing.T) (*config.Context, UserAPIKeyService) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "fedcba9876543210fedcba9876543210")
	_, ctx := testutil.NewTestServer()
	return ctx, NewUserAPIKeyService(ctx)
}

// countUserAPIKeys returns how many rows exist for (uid, space, client),
// regardless of status — used to prove GetOrCreate does not duplicate.
func countUserAPIKeys(t *testing.T, ctx *config.Context, uid, spaceID, clientID string) int {
	var n int
	err := ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM user_api_key WHERE uid=? AND space_id=? AND client_id=?",
		uid, spaceID, clientID,
	).LoadOne(&n)
	require.NoError(t, err)
	return n
}

func userAPIKeyHashForTest(t *testing.T, s string) string {
	t.Helper()
	hash, err := hashUserAPIKey(s)
	require.NoError(t, err)
	return hash
}

func TestUserAPIKeyService_GetOrCreate_Idempotent(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]

	first, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(first, UserAPIKeyPrefix), "key should carry uk_ prefix, got %q", first)

	second, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	assert.Equal(t, first, second, "repeated GetOrCreate must echo the same plaintext key")
	assert.Equal(t, 1, countUserAPIKeys(t, ctx, uid, spaceID, clientIDBotFather), "must not create a duplicate row")
}

func TestUserAPIKeyService_GetOrCreate_StoresVerifierHashAndCipherOnly(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "api-key-owner")

	key, err := svc.GetOrCreate(uid, spaceID, "octopush")
	require.NoError(t, err)

	var row struct {
		APIKey       string
		APIKeyHash   string
		APIKeyCipher string
	}
	err = ctx.DB().Select("api_key", "api_key_hash", "api_key_cipher").
		From("user_api_key").
		Where("uid=? AND space_id=? AND client_id=?", uid, spaceID, "octopush").
		LoadOne(&row)
	require.NoError(t, err)
	assert.NotEqual(t, key, row.APIKey, "api_key column must not store the bearer credential")
	assert.Equal(t, userAPIKeyHashForTest(t, key), row.APIKeyHash)
	assert.NotEmpty(t, row.APIKeyCipher)
	assert.NotContains(t, row.APIKeyCipher, key)

	auth, err := svc.AuthByKey(key)
	require.NoError(t, err)
	require.NotNil(t, auth)
	assert.Equal(t, uid, auth.UID)
}

func TestUserAPIKeyService_GetOrCreate_UsesUserAPIKeySecretNotOctoMasterKey(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "api-key-owner")
	t.Setenv("OCTO_MASTER_KEY", "")

	key, err := svc.GetOrCreate(uid, spaceID, "octopush")
	require.NoError(t, err)

	auth, err := svc.AuthByKey(key)
	require.NoError(t, err)
	require.NotNil(t, auth)
	assert.Equal(t, uid, auth.UID)
	assert.Equal(t, "octopush", auth.ClientID)
}

func TestUserAPIKeyService_GetOrCreate_BotFatherDoesNotRequireUserAPIKeySecret(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "botfather-owner")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "")

	key, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(key, UserAPIKeyPrefix))

	auth, err := svc.AuthByKey(key)
	require.NoError(t, err)
	require.NotNil(t, auth)
	assert.Equal(t, clientIDBotFather, auth.ClientID)
}

func TestUserAPIKeyService_GetOrCreate_DistinctPerClient(t *testing.T) {
	_, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]

	bf, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	octopush, err := svc.GetOrCreate(uid, spaceID, "octopush")
	require.NoError(t, err)

	assert.NotEqual(t, bf, octopush, "different client_id under same uid+space must get distinct keys")
}

// GetOrCreate with a blank clientID must default to the botfather client, so
// it shares the same key the /quickstart flow produces.
func TestUserAPIKeyService_GetOrCreate_BlankClientDefaultsBotFather(t *testing.T) {
	_, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]

	blank, err := svc.GetOrCreate(uid, spaceID, "")
	require.NoError(t, err)
	explicit, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)
	assert.Equal(t, blank, explicit)
}

// Empty spaceID maps to the legacy no-space row and is still idempotent.
func TestUserAPIKeyService_GetOrCreate_EmptySpace(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]

	first, err := svc.GetOrCreate(uid, "", clientIDBotFather)
	require.NoError(t, err)
	second, err := svc.GetOrCreate(uid, "", clientIDBotFather)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, countUserAPIKeys(t, ctx, uid, "", clientIDBotFather))
}

// Concurrent GetOrCreate on the same (uid, space, client): the unique key
// forces all-but-one INSERT to collide, and the duplicate-key fallback must
// re-read the winning row — so every caller returns the SAME plaintext key and
// exactly one row exists. This exercises the H2 fallback path deterministically.
func TestUserAPIKeyService_GetOrCreate_ConcurrentNoDuplicate(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]

	const n = 8
	keys := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			keys[i], errs[i] = svc.GetOrCreate(uid, spaceID, "octopush")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "concurrent GetOrCreate #%d", i)
		assert.Equal(t, keys[0], keys[i], "all concurrent callers must converge on one key")
	}
	assert.Equal(t, 1, countUserAPIKeys(t, ctx, uid, spaceID, "octopush"), "concurrent create must not duplicate rows")
}

func TestUserAPIKeyService_GetOrCreate_RotatesRevokedRow(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	clientID := "octopush"
	insertTestUser(t, ctx, uid, "api-key-owner")

	oldKey, err := svc.GetOrCreate(uid, spaceID, clientID)
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE user_api_key SET status=?, revoked_at=NOW() WHERE api_key_hash=?",
		userAPIKeyStatusRevoked, userAPIKeyHashForTest(t, oldKey),
	).Exec()
	require.NoError(t, err)

	newKey, err := svc.GetOrCreate(uid, spaceID, clientID)
	require.NoError(t, err)
	assert.NotEqual(t, oldKey, newKey, "reactivating a revoked row must rotate the plaintext key")
	assert.Equal(t, 1, countUserAPIKeys(t, ctx, uid, spaceID, clientID), "revoked-row recovery must not insert a duplicate")

	oldAuth, err := svc.AuthByKey(oldKey)
	require.NoError(t, err)
	assert.Nil(t, oldAuth, "old revoked key must stay invalid after rotation")

	newAuth, err := svc.AuthByKey(newKey)
	require.NoError(t, err)
	require.NotNil(t, newAuth)
	assert.Equal(t, uid, newAuth.UID)
	assert.Equal(t, spaceID, newAuth.SpaceID)
	assert.Equal(t, clientID, newAuth.ClientID)

	var activeCleared int
	err = ctx.DB().SelectBySql(
		"SELECT COUNT(*) FROM user_api_key WHERE uid=? AND space_id=? AND client_id=? AND status=? AND revoked_at IS NULL",
		uid, spaceID, clientID,
		userAPIKeyStatusActive,
	).LoadOne(&activeCleared)
	require.NoError(t, err)
	assert.Equal(t, 1, activeCleared, "rotated active row must clear revoked_at")
}

func TestUserAPIKeyService_GetOrCreate_ConcurrentRevokedRowIsIdempotent(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	clientID := "octopush"

	oldKey, err := svc.GetOrCreate(uid, spaceID, clientID)
	require.NoError(t, err)
	_, err = ctx.DB().UpdateBySql(
		"UPDATE user_api_key SET status=?, revoked_at=NOW() WHERE api_key_hash=?",
		userAPIKeyStatusRevoked, userAPIKeyHashForTest(t, oldKey),
	).Exec()
	require.NoError(t, err)

	const n = 16
	keys := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			keys[i], errs[i] = svc.GetOrCreate(uid, spaceID, clientID)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "concurrent revoked-row GetOrCreate #%d", i)
		assert.Equal(t, keys[0], keys[i], "all callers must echo the reactivated key")
	}
	assert.NotEqual(t, oldKey, keys[0], "revoked row recovery must rotate the old plaintext key")
	assert.Equal(t, 1, countUserAPIKeys(t, ctx, uid, spaceID, clientID), "revoked-row recovery must keep one logical row")
}

func TestIsDuplicateKeyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate entry message", errors.New("Error 1062: Duplicate entry 'u1-s1-octopush' for key 'uk_uid_space_client'"), true},
		{"bare 1062", errors.New("mysql: 1062 duplicate"), true},
		{"unrelated error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDuplicateKeyErr(tc.err))
		})
	}
}

func TestUserAPIKeyService_AuthByKey(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "api-key-owner")

	key, err := svc.GetOrCreate(uid, spaceID, "octopush")
	require.NoError(t, err)

	got, err := svc.AuthByKey(key)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uid, got.UID)
	assert.Equal(t, spaceID, got.SpaceID)
	assert.Equal(t, "octopush", got.ClientID)
	assert.Equal(t, key, got.APIKey)

	// Unknown key resolves to (nil, nil).
	unknown, err := svc.AuthByKey("uk_does_not_exist")
	require.NoError(t, err)
	assert.Nil(t, unknown)

	// Revoked key (status=0) must fail auth.
	_, err = ctx.DB().UpdateBySql(
		"UPDATE user_api_key SET status=? WHERE api_key_hash=?", userAPIKeyStatusRevoked, userAPIKeyHashForTest(t, key),
	).Exec()
	require.NoError(t, err)

	revoked, err := svc.AuthByKey(key)
	require.NoError(t, err)
	assert.Nil(t, revoked, "revoked key must not authenticate")
}

func TestUserAPIKeyService_AuthByKeyLegacyUpgradeFailureIsBestEffort(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	blockerUID := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "legacy-owner")
	insertTestUser(t, ctx, blockerUID, "hash-blocker")

	legacyKey := UserAPIKeyPrefix + "legacy_" + util.GenerUUID()[:8]
	storedAPIKey := storedUserAPIKeyValue(userAPIKeyHashForTest(t, legacyKey))
	_, err := ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "api_key_hash", "api_key_cipher", "space_id", "client_id", "status").
		Values(uid, legacyKey, "", "", spaceID, "octopush", userAPIKeyStatusActive).
		Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "api_key_hash", "api_key_cipher", "space_id", "client_id", "status").
		Values(blockerUID, storedAPIKey, "", "", "s_"+util.GenerUUID()[:8], clientIDBotFather, userAPIKeyStatusActive).
		Exec()
	require.NoError(t, err)

	got, err := svc.AuthByKey(legacyKey)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uid, got.UID)
	assert.Equal(t, "octopush", got.ClientID)
	assert.Equal(t, legacyKey, got.APIKey)
}

func TestUserAPIKeyService_AuthByKeyLegacyPlaintextDoesNotNeedOctoMasterKey(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "legacy-owner")
	t.Setenv("OCTO_MASTER_KEY", "")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "")

	legacyKey := UserAPIKeyPrefix + "legacy_" + util.GenerUUID()[:8]
	_, err := ctx.DB().InsertInto("user_api_key").
		Columns("uid", "api_key", "api_key_hash", "api_key_cipher", "space_id", "client_id", "status").
		Values(uid, legacyKey, "", "", spaceID, clientIDBotFather, userAPIKeyStatusActive).
		Exec()
	require.NoError(t, err)

	got, err := svc.AuthByKey(legacyKey)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uid, got.UID)
	assert.Equal(t, legacyKey, got.APIKey)
}

func TestUserAPIKeyService_AuthByKeyRejectsInactiveUser(t *testing.T) {
	ctx, svc := setupAPIKeyServiceTest(t)
	uid := "u_" + util.GenerUUID()[:8]
	spaceID := "s_" + util.GenerUUID()[:8]
	insertTestUser(t, ctx, uid, "api-key-owner")

	key, err := svc.GetOrCreate(uid, spaceID, clientIDBotFather)
	require.NoError(t, err)

	_, err = ctx.DB().Update("user").Set("status", 0).Where("uid=?", uid).Exec()
	require.NoError(t, err)

	got, err := svc.AuthByKey(key)
	require.NoError(t, err)
	assert.Nil(t, got, "active uk_ rows must not authenticate inactive users")
}

func TestDecryptUserAPIKeyRejectsUnknownCipherPrefix(t *testing.T) {
	got, err := decryptUserAPIKey("plain-legacy-value")
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestUserAPIKeyClientDimensionDownDoesNotHardDeleteIntegrationKeys(t *testing.T) {
	raw, err := os.ReadFile("sql/20260603000001_botfather_legacy01.sql")
	require.NoError(t, err)
	sql := string(raw)

	assert.NotContains(t, sql, "DELETE FROM `user_api_key` WHERE `client_id` <> 'botfather'")
	assert.Contains(t, sql, "SIGNAL SQLSTATE", "rollback must abort loudly instead of silently deleting integration keys")
}
