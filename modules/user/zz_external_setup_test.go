// External test package for modules/user that pulls in the full module
// registry via blank-importing internal. Reason: tests under this package
// run alongside in-package user tests (which are `package user`); when
// they share a test binary with packages like bot_provision_test that
// already blank-import internal, gorp_migrations gets populated with
// the full module set, and an in-package user test (whose own transitive
// deps cover only a 10-module subset) would panic on the extra migration
// rows as "orphans".
//
// Adding this single external-package file forces user's test binary to
// also register every module via init(), making migration plans match
// what's actually in gorp_migrations regardless of test-binary ordering.
// External package + blank-import internal does NOT create a cycle:
// internal → modules/user (production package), and this file is in
// modules/user_test (external test package), which production never sees.
//
// No TestMain here — main_test.go already provides one in the in-package
// `user` test set; adding a second would conflict ("multiple definitions
// of TestMain"). A bare blank-import in any test file is enough to
// trigger init() at test binary load.
//
// Supersedes the deprecated deps_test.go (removed in v3.4 cleanup): that
// file only blank-imported a 4-module subset (base/botfather/group/robot)
// which is a strict subset of what `internal` already pulls. Keeping
// both was redundant — this single file fully covers the migration
// registry requirement.
package user_test

import (
	_ "github.com/Mininglamp-OSS/octo-server/internal"
)
