package botevent

import (
	"fmt"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	rd "github.com/go-redis/redis"
)

// This package's tests are almost all integration tests: the contract is about what
// two concurrent processes and one exclusive cursor do to each other, and a fake would
// only restate the code. So a missing MySQL or Redis has to be handled, and there are
// exactly two right answers depending on where the tests run.
//
// Locally, skipping is the ergonomic answer — not everybody has both containers up to
// change a comment.
//
// In CI it is the *wrong* answer, and quietly so (review P2-11). Every seqTestCtx call
// site used to t.Skipf on an unusable dependency, so a credential change, a renamed
// service or a dropped database would leave only the source guards running while
// `go test` still exited 0. That is the same failure mode as the `botevent_test`
// database an earlier revision of this package used: indistinguishable from passing,
// on the tests that carry the whole safety argument.
//
// So the dependencies are probed once here, and CI (which sets CI=true, as GitHub
// Actions does for every job) gets a hard failure naming what is missing.
func TestMain(m *testing.M) {
	if err := probeTestDependencies(); err != nil {
		if os.Getenv("CI") != "" {
			fmt.Fprintf(os.Stderr, "pkg/botevent: %v\n\n"+
				"This package's integration tests carry the #697 safety argument (seeding above "+
				"every ceiling, rollback detection, activation gating). Skipping them in CI would "+
				"look exactly like passing, so this is a failure instead. Set OCTO_TEST_MYSQL_ADDR "+
				"/ OCTO_TEST_REDIS_ADDR if the services are not on their default addresses.\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "pkg/botevent: %v — integration tests will skip (set CI=true to "+
			"make this a failure)\n", err)
	}
	os.Exit(m.Run())
}

// probeTestDependencies reports the first missing dependency, if any.
//
// It performs the same two CREATE TABLEs seqTestCtx does, because those are what actually
// gate the tests: probing only `SELECT 1` and `PING` left the interesting failure — a
// privilege or collation problem on the writes — skipping every integration test while
// `go test` exited 0, which is the exact blind spot this TestMain was added to close
// (review, fifth round).
func probeTestDependencies() error {
	mysqlAddr, redisAddr := testAddrs()

	cfg := config.New()
	cfg.DB.MySQLAddr = mysqlAddr
	ctx := config.NewContext(cfg)
	if _, err := ctx.DB().Exec("SELECT 1"); err != nil {
		return fmt.Errorf("no usable MySQL at %s: %w", mysqlAddr, err)
	}
	if _, err := ctx.DB().Exec(seqTableDDL); err != nil {
		return fmt.Errorf("cannot create the `seq` table at %s: %w", mysqlAddr, err)
	}
	if _, err := ctx.DB().Exec(stateTableDDL); err != nil {
		return fmt.Errorf("cannot create %s at %s: %w", stateTable, mysqlAddr, err)
	}

	client := rd.NewClient(&rd.Options{Addr: redisAddr})
	defer func() { _ = client.Close() }()
	if err := client.Ping().Err(); err != nil {
		return fmt.Errorf("no usable Redis at %s: %w", redisAddr, err)
	}
	return nil
}
