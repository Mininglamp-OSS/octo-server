package botfather

import (
	"errors"
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
		"UPDATE user_api_key SET status=? WHERE api_key=?", userAPIKeyStatusRevoked, key,
	).Exec()
	require.NoError(t, err)

	revoked, err := svc.AuthByKey(key)
	require.NoError(t, err)
	assert.Nil(t, revoked, "revoked key must not authenticate")
}
