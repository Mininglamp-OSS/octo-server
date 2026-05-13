package statistics

// test_deps_setup_test.go ensures every module whose migrations are
// referenced by transitively-pulled migrations registers itself via its
// init(). Without these blank imports testutil.NewTestServer fails the
// space module's 20260308000002_space_legacy01.sql migration with
// "Table 'test.robot' doesn't exist", because space (transitively
// imported via group → user → space) JOINs the robot table — but the
// robot package, where that table is created, is not pulled in by this
// test package's production code.
//
// Mirrors the pattern used in modules/botfather/api_bot_group_test.go.

import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
)
