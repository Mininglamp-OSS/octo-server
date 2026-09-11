package user

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	rd "github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenHTTPFixtureReleasesResources(t *testing.T) {
	var pool *sql.DB
	var client *rd.Client
	var databaseName string
	require.True(t, t.Run("fixture", func(t *testing.T) {
		_, ctx, _, _ := newTokenHTTPTestServer(t)
		pool = ctx.DB().DB
		_, client = auth.SessionStoreAndClientForContext(ctx)
		require.NoError(t, pool.QueryRow("SELECT DATABASE()").Scan(&databaseName))
		require.NoError(t, client.Ping().Err())
	}))

	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assert.Error(t, pool.PingContext(deadline), "the fixture must close its MySQL pool before returning")
	assert.Error(t, client.Ping().Err(), "the fixture must close its Redis session client before returning")
	admin, err := sql.Open("mysql", "root:demo@tcp(127.0.0.1:3306)/information_schema?charset=utf8mb4&parseTime=true")
	require.NoError(t, err)
	defer admin.Close()
	var remaining int
	require.NoError(t, admin.QueryRowContext(deadline,
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", databaseName).Scan(&remaining))
	assert.Zero(t, remaining, "the fixture must delete only its own temporary database when its test ends")
}
