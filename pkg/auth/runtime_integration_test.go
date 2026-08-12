package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/require"
)

func TestInitializeSessionRolloutUsesMySQLAuthorityAcrossBoots(t *testing.T) {
	configureRolloutIntegrationEnv(t)
	dsn := newRolloutIntegrationDatabase(t)
	tokenPrefix := "auth-runtime-token:" + util.GenerUUID() + ":"
	uidPrefix := "auth-runtime-uid:" + util.GenerUUID() + ":"

	first := newRolloutIntegrationContext(t, dsn, tokenPrefix, uidPrefix)
	store := SessionStoreForContext(first)
	boot, control, err := InitializeSessionRollout(first)
	require.NoError(t, err)
	require.Equal(t, RolloutBootFresh, boot.Outcome)
	require.Equal(t, SessionModeExpand, boot.Floor)
	require.Equal(t, defaultRecoveryMaxPerUID, boot.MaxPerUID)
	require.ErrorIs(t, store.CanIssue(), ErrWriterLeaseLost,
		"initialization applies reader state but must not un-fence before registry publication")

	state, err := control.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, SessionModeExpand, state.Floor)
	require.EqualValues(t, 1, state.Version)

	cachedBoot, cachedControl, err := InitializeSessionRollout(first)
	require.NoError(t, err)
	require.Equal(t, boot, cachedBoot)
	require.Same(t, control, cachedControl)
	recordedBoot, warnings := SessionBootForContext(first)
	require.Equal(t, boot, recordedBoot)
	require.NotEmpty(t, warnings, "explicitly empty legacy env values are reported instead of silently guessed")

	second := newRolloutIntegrationContext(t, dsn, tokenPrefix, uidPrefix)
	secondStore := SessionStoreForContext(second)
	normal, secondControl, err := InitializeSessionRollout(second)
	require.NoError(t, err)
	require.Equal(t, RolloutBootNormal, normal.Outcome)
	require.Equal(t, state.Floor, normal.Floor)
	require.Equal(t, state.MaxPerUID, normal.MaxPerUID)
	require.ErrorIs(t, secondStore.CanIssue(), ErrWriterLeaseLost)

	secondState, err := secondControl.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, state.Floor, secondState.Floor)
	require.Equal(t, state.Version, secondState.Version)
}

func TestInitializeSessionRolloutAdoptsLegacyRedisOnlyOnce(t *testing.T) {
	configureRolloutIntegrationEnv(t)
	dsn := newRolloutIntegrationDatabase(t)
	tokenPrefix := "auth-adopt-token:" + util.GenerUUID() + ":"
	uidPrefix := "auth-adopt-uid:" + util.GenerUUID() + ":"

	first := newRolloutIntegrationContext(t, dsn, tokenPrefix, uidPrefix)
	store := SessionStoreForContext(first)
	legacy := `{"mode_floor":"revoke","writer_version":3,"observation_min_gap_ms":0,"max_per_uid":7}`
	require.NoError(t, store.Client().Set(store.rolloutControlKey(), legacy, 0).Err())

	boot, _, err := InitializeSessionRollout(first)
	require.NoError(t, err)
	require.Equal(t, RolloutBootAdopted, boot.Outcome)
	require.Equal(t, SessionModeRevoke, boot.Floor)
	require.Equal(t, 7, boot.MaxPerUID)

	// Once MySQL owns the singleton, even an invalid legacy Redis record is not
	// consulted on the next process boot.
	require.NoError(t, store.Client().Set(store.rolloutControlKey(), "not-json", 0).Err())
	second := newRolloutIntegrationContext(t, dsn, tokenPrefix, uidPrefix)
	SessionStoreForContext(second)
	normal, _, err := InitializeSessionRollout(second)
	require.NoError(t, err)
	require.Equal(t, RolloutBootNormal, normal.Outcome)
	require.Equal(t, SessionModeRevoke, normal.Floor)
	require.Equal(t, 7, normal.MaxPerUID)
}

func TestInitializeSessionRolloutSerializesConcurrentCallers(t *testing.T) {
	configureRolloutIntegrationEnv(t)
	dsn := newRolloutIntegrationDatabase(t)
	ctx := newRolloutIntegrationContext(t, dsn,
		"auth-concurrent-token:"+util.GenerUUID()+":",
		"auth-concurrent-uid:"+util.GenerUUID()+":",
	)
	SessionStoreForContext(ctx)

	const callers = 32
	start := make(chan struct{})
	boots := make([]RolloutBoot, callers)
	controls := make([]*RolloutControlStore, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			boots[index], controls[index], errs[index] = InitializeSessionRollout(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i], "caller %d", i)
		require.Equal(t, boots[0], boots[i], "caller %d", i)
		require.Same(t, controls[0], controls[i], "caller %d", i)
	}
	recorded, _ := SessionBootForContext(ctx)
	require.Equal(t, boots[0], recorded)
}

func configureRolloutIntegrationEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OCTO_AUTH_SESSION_MODE",
		"OCTO_AUTH_SESSION_MAX_PER_UID",
		"OCTO_AUTH_SESSION_CANARY_AHEAD",
		"OCTO_AUTH_SESSION_EXPECT_WRITERS",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("OCTO_AUTH_SESSION_AUTO_ADVANCE", "0")
}

func newRolloutIntegrationDatabase(t *testing.T) string {
	t.Helper()
	admin, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	databaseName := "auth_rollout_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
	_, err = admin.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := admin.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
		require.NoError(t, dropErr)
	})

	dsn := fmt.Sprintf("root:demo@tcp(127.0.0.1:3306)/%s?charset=utf8mb4&parseTime=true&multiStatements=true", databaseName)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE octo_session_rollout_state (
			singleton_id TINYINT UNSIGNED NOT NULL PRIMARY KEY,
			floor VARCHAR(16) NOT NULL,
			max_per_uid INT UNSIGNED NOT NULL,
			version BIGINT UNSIGNED NOT NULL DEFAULT 1,
			paused TINYINT(1) NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
		) ENGINE=InnoDB;
		CREATE TABLE octo_session_rollout_advance (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			from_floor VARCHAR(16) NOT NULL DEFAULT '',
			to_floor VARCHAR(16) NOT NULL,
			actor VARCHAR(128) NOT NULL,
			evidence_json JSON NULL,
			redis_run_id VARCHAR(64) NOT NULL DEFAULT '',
			writer_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
			transition_kind VARCHAR(32) NOT NULL DEFAULT 'advance',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB
	`)
	require.NoError(t, err)
	return dsn
}

func newRolloutIntegrationContext(t *testing.T, dsn, tokenPrefix, uidPrefix string) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.DB.MySQLAddr = dsn
	cfg.Cache.TokenCachePrefix = tokenPrefix
	cfg.Cache.UIDTokenCachePrefix = uidPrefix
	ctx := config.NewContext(cfg)
	t.Cleanup(func() {
		store, client := SessionStoreAndClientForContext(ctx)
		keys, _ := client.Keys(store.UIDTokenPrefix() + "*").Result()
		tokenKeys, _ := client.Keys(tokenPrefix + "*").Result()
		keys = append(keys, tokenKeys...)
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
		_ = client.Close()
		_ = ctx.DB().DB.Close()
	})
	return ctx
}
