package opanalytics

// test_deps_setup_test.go blank-imports the modules whose migrations create the
// source tables the ETL/读侧 query (user, group/group_member, space/space_member,
// robot). Without these, module.Setup (invoked by testutil.NewTestServer) would not
// register their SQLDir migrations and the test schema would be missing those tables.
// The `message`/`message1..N` shard tables are NOT created by any migration (WuKongIM
// owns them in prod); the integration test creates them itself (see api_test.go).
import (
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)
