// Package backup is a tombstone for the removed built-in WuKongIM backup
// module (issue #139). The Go code (cron scheduler, /v1/manager/backup/*
// API, tar+COS storage, disk precheck — ~1.6k LoC) has been deleted; only
// the two historical SQL migrations remain so sql-migrate continues to
// recognise their IDs.
//
// Why a tombstone instead of deleting the SQL files outright:
//   - Deleting them would leave orphaned IDs in gorp_migrations on existing
//     deployments — sql-migrate's PlanMigration stage panics with "unknown
//     migration in database" the moment it sees an ID with no embedded file.
//   - Purging those rows at startup would let a rollback to a pre-removal
//     release silently re-run the original CREATE TABLE statements (no
//     IF NOT EXISTS) and fail at startup with MySQL 1050 because
//     backup_config / backup_history are intentionally kept on disk.
//
// SetupAPI is intentionally nil — no HTTP surface is registered. Only the
// SQLDir is exposed so the embedded migrations stay discoverable.
//
// The backup_config / backup_history tables themselves are deliberately
// left in place (per issue #139's removal plan) so existing production
// rows are not lost. A future change may add a drop migration once we are
// confident no deployment depends on the data.
package backup

import (
	"embed"

	"github.com/Mininglamp-OSS/octo-lib/pkg/register"
)

//go:embed sql
var sqlFS embed.FS

func init() {
	register.AddModule(func(ctx interface{}) register.Module {
		return register.Module{
			Name:   "backup",
			SQLDir: register.NewSQLFS(sqlFS),
		}
	})
}
