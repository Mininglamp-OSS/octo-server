//go:build integration

package message

// =============================================================================
// E2E coverage for the production DefaultFollowedGroupGuard wiring.
//
// The unit tests in default_followed_group_guard_test.go stub both stages of
// the guard.  This file wires the REAL production chain:
//
//   convext.Service.AuthorizeAndMaterializeDefaultFollowedGroups
//     └── defaultFollowedGroupGuard (Stage 1 = groupCategoryDB JOIN,
//                                    Stage 2 = threadAuthChecker.checkChannelAccess)
//           ├── groupCategoryDB.FilterDefaultFollowedGroups
//           │     └── INNER JOIN group_setting × group_category (status != 2)
//           └── threadAuthChecker.AuthorizeChannelFollow
//                 └── group.IService.ExistMember
//                     group.IService.GetGroups
//                     group.DB.QueryExternalGroupNosForUser
//                 └── all against real MySQL rows
//
// This exercises every rejection reason the production chain has, with the
// actual DB driver, JOIN ordering, and group/group_setting/group_member
// schema in play.  It is the integration-level regression test for issue
// #151 reviews #1 + #2 + re-review H1 (soft-deleted category gap).
// =============================================================================

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	convext "github.com/Mininglamp-OSS/octo-server/modules/conversation_ext"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureGuardE2ETables creates the minimal schemas needed for the production
// guard chain on the integration MySQL.  conv_ext_test ships with
// `user_conversation_ext`, `user_follow_version`, and `group_category`; this
// helper adds the three tables only the group/group_setting access paths read
// (`group`, `group_member`, `group_setting`) so the test does not depend on
// running the full octo migration suite.
//
// DROP + CREATE rather than CREATE IF NOT EXISTS so schema tweaks (e.g. the
// explicit COLLATE on group_setting.category_id added for issue #150
// compatibility) actually take effect on a re-run; the tables only carry
// per-test data so the destructive DDL is safe.
func ensureGuardE2ETables(t *testing.T, ctx *config.Context) {
	t.Helper()
	for _, tbl := range []string{"group_setting", "group_member", "`group`"} {
		_, err := ctx.DB().Exec("DROP TABLE IF EXISTS " + tbl)
		require.NoError(t, err, "drop %s", tbl)
	}
	stmts := []string{
		// modules/group/sql/20191106000002 + 20260307000004 + 20260424000001
		// (only the columns this test touches).
		"CREATE TABLE `group` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `name` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `creator` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `status` SMALLINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `forbidden` SMALLINT NOT NULL DEFAULT 0," +
			"  `invite` SMALLINT NOT NULL DEFAULT 0," +
			"  `forbidden_add_friend` SMALLINT NOT NULL DEFAULT 0," +
			"  `allow_view_history_msg` SMALLINT NOT NULL DEFAULT 1," +
			"  `allow_member_pinned_message` SMALLINT NOT NULL DEFAULT 0," +
			"  `category` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `group_type` SMALLINT NOT NULL DEFAULT 0," +
			"  `notice` VARCHAR(200) NOT NULL DEFAULT ''," +
			"  `avatar` VARCHAR(200) NOT NULL DEFAULT ''," +
			"  `allow_external` SMALLINT NOT NULL DEFAULT 1," +
			"  `group_md` TEXT," +
			"  `group_md_version` BIGINT NOT NULL DEFAULT 0," +
			"  `group_md_updated_at` TIMESTAMP NULL DEFAULT NULL," +
			"  `group_md_updated_by` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `space_id` VARCHAR(40) DEFAULT ''," +
			"  `is_external_group` SMALLINT NOT NULL DEFAULT 0," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_groupNo` (`group_no`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE `group_member` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `remark` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `role` SMALLINT NOT NULL DEFAULT 0," +
			"  `version` BIGINT NOT NULL DEFAULT 0," +
			"  `is_deleted` SMALLINT NOT NULL DEFAULT 0," +
			"  `status` SMALLINT NOT NULL DEFAULT 1," +
			"  `vercode` VARCHAR(100) NOT NULL DEFAULT ''," +
			"  `robot` SMALLINT NOT NULL DEFAULT 0," +
			"  `invite_uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `forbidden_expir_time` BIGINT NOT NULL DEFAULT 0," +
			"  `bot_admin` SMALLINT NOT NULL DEFAULT 0," +
			"  `is_external` SMALLINT NOT NULL DEFAULT 0," +
			"  `source_space_id` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `group_no_uid` (`group_no`, `uid`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		// modules/message/sql/.../category_legacy01 — group_setting columns
		// the guard reads (category_id, category_sort + uid/group_no key).
		//
		// IMPORTANT: category_id must explicitly use the same COLLATE as
		// group_category.category_id (utf8mb4_general_ci, established by
		// category_legacy01.sql line 15).  MySQL 8.0 silently defaults missing
		// COLLATE clauses on CREATE TABLE to utf8mb4_0900_ai_ci, which is
		// exactly the production schema drift reported in issue #150 —
		// without the explicit COLLATE here the INNER JOIN in Stage 1
		// (FilterDefaultFollowedGroups) errors with "Error 1267 (HY000):
		// Illegal mix of collations".  Pinning it here lets the guard test
		// pass on conv_ext_test until #150's forward-repair migration lands.
		"CREATE TABLE `group_setting` (" +
			"  `id` INT NOT NULL AUTO_INCREMENT PRIMARY KEY," +
			"  `uid` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `group_no` VARCHAR(40) NOT NULL DEFAULT ''," +
			"  `category_id` VARCHAR(32) COLLATE utf8mb4_general_ci DEFAULT NULL," +
			"  `category_sort` INT NOT NULL DEFAULT 0," +
			"  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP," +
			"  UNIQUE KEY `uk_uid_groupno` (`uid`, `group_no`)" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci",
	}
	for _, s := range stmts {
		_, err := ctx.DB().Exec(s)
		require.NoError(t, err, "ensureGuardE2ETables: %s", s[:60])
	}
}

// cleanGuardE2ERows wipes only rows this test owns.  Aggressive `DELETE FROM
// <table>` would race with other integration tests that share the
// conv_ext_test database (e.g. conversation_ext's UpdateSort suite seeds its
// own user_conversation_ext + user_follow_version rows).  All this test's
// data lives under the e2e- / cat- prefixes, so per-table prefix DELETEs
// keep the shared DB isolated when both packages run side by side.
func cleanGuardE2ERows(t *testing.T, ctx *config.Context) {
	t.Helper()
	deletes := []struct{ tbl, where, prefix string }{
		{"user_conversation_ext", "uid LIKE ?", "e2e-%"},
		{"user_follow_version", "uid LIKE ?", "e2e-%"},
		{"group_category", "uid LIKE ?", "e2e-%"},
		{"group_setting", "uid LIKE ?", "e2e-%"},
		{"group_member", "uid LIKE ?", "e2e-%"},
		{"`group`", "group_no LIKE ?", "e2e-%"},
	}
	for _, d := range deletes {
		_, err := ctx.DB().Exec("DELETE FROM "+d.tbl+" WHERE "+d.where, d.prefix)
		require.NoError(t, err, "clean %s", d.tbl)
	}
}

func seedGroupRow(t *testing.T, ctx *config.Context, groupNo, spaceID string, status int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO `group` (group_no, status, space_id) VALUES (?, ?, ?)",
		groupNo, status, spaceID,
	)
	require.NoError(t, err, "seedGroupRow %s", groupNo)
}

func seedGroupMember(t *testing.T, ctx *config.Context, groupNo, uid string, isExternal int, sourceSpaceID string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_member (group_no, uid, is_deleted, status, is_external, source_space_id) VALUES (?, ?, 0, 1, ?, ?)",
		groupNo, uid, isExternal, sourceSpaceID,
	)
	require.NoError(t, err, "seedGroupMember %s/%s", groupNo, uid)
}

func seedGroupCategory(t *testing.T, ctx *config.Context, categoryID, uid string, status int) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT INTO group_category (category_id, space_id, uid, name, sort, status) VALUES (?, '', ?, 'cat', 0, ?)",
		categoryID, uid, status,
	)
	require.NoError(t, err, "seedGroupCategory %s", categoryID)
}

func seedGroupSetting(t *testing.T, ctx *config.Context, uid, groupNo, categoryID string) {
	t.Helper()
	var cat interface{}
	if categoryID == "" {
		cat = nil
	} else {
		cat = categoryID
	}
	_, err := ctx.DB().Exec(
		"INSERT INTO group_setting (uid, group_no, category_id) VALUES (?, ?, ?)",
		uid, groupNo, cat,
	)
	require.NoError(t, err, "seedGroupSetting %s/%s", uid, groupNo)
}

// TestE2E_DefaultFollowedGroupGuard_ProductionChain wires the real production
// objects (group.Service, threadAuthChecker, defaultFollowedGroupGuard,
// conversation_ext.Service) and asserts every rejection / acceptance path
// against real MySQL rows.
func TestE2E_DefaultFollowedGroupGuard_ProductionChain(t *testing.T) {
	ctx := newSidebarIntegCtx(t)
	ensureGuardE2ETables(t, ctx)
	cleanGuardE2ERows(t, ctx)

	const (
		uid                  = "e2e-u1"
		spaceA               = "e2e-sA"
		spaceB               = "e2e-sB"
		gOK                  = "e2e-g-ok"           // member + spaceA + category → MATERIALIZE
		gDisband             = "e2e-g-disband"      // member + spaceA + category but Disband → REJECT
		gWrongSpace          = "e2e-g-wrong"        // member + spaceB (not request space) + category → REJECT
		gNotMember           = "e2e-g-notmem"       // spaceA + category but uid is NOT a member → REJECT
		gNoCategory          = "e2e-g-nocat"        // member + spaceA but no category → REJECT (Stage 1)
		gSoftDeletedCat      = "e2e-g-softcat"      // member + spaceA + category_id but category soft-deleted → REJECT (Stage 1, H1 regression)
		gExternal            = "e2e-g-external"     // external member in spaceB referencing spaceA + category → MATERIALIZE
		gFake                = "e2e-g-fake"         // attacker-injected, no rows anywhere → REJECT (Stage 1)
		catLive              = "cat-live"
		catSoftDeleted       = "cat-deleted"
	)

	// --- groups ---
	seedGroupRow(t, ctx, gOK, spaceA, group.GroupStatusNormal)
	seedGroupRow(t, ctx, gDisband, spaceA, group.GroupStatusDisband)
	seedGroupRow(t, ctx, gWrongSpace, spaceB, group.GroupStatusNormal)
	seedGroupRow(t, ctx, gNotMember, spaceA, group.GroupStatusNormal)
	seedGroupRow(t, ctx, gNoCategory, spaceA, group.GroupStatusNormal)
	seedGroupRow(t, ctx, gSoftDeletedCat, spaceA, group.GroupStatusNormal)
	seedGroupRow(t, ctx, gExternal, spaceB, group.GroupStatusNormal)
	// gFake is intentionally absent from the group table.

	// --- membership ---
	seedGroupMember(t, ctx, gOK, uid, 0, "")
	seedGroupMember(t, ctx, gDisband, uid, 0, "")
	seedGroupMember(t, ctx, gWrongSpace, uid, 0, "")
	// gNotMember: NO group_member row for uid.
	seedGroupMember(t, ctx, gNoCategory, uid, 0, "")
	seedGroupMember(t, ctx, gSoftDeletedCat, uid, 0, "")
	// External-member fallback: uid joined gExternal from spaceA (sourceSpaceID==spaceA).
	seedGroupMember(t, ctx, gExternal, uid, 1, spaceA)

	// --- categories ---
	seedGroupCategory(t, ctx, catLive, uid, 1)       // status=1 normal
	seedGroupCategory(t, ctx, catSoftDeleted, uid, 2) // status=2 soft-deleted (H1)

	// --- group_setting (per-user category assignment) ---
	seedGroupSetting(t, ctx, uid, gOK, catLive)
	seedGroupSetting(t, ctx, uid, gDisband, catLive)
	seedGroupSetting(t, ctx, uid, gWrongSpace, catLive)
	seedGroupSetting(t, ctx, uid, gNotMember, catLive)
	// gNoCategory: row exists but category_id IS NULL → Stage 1 drops.
	seedGroupSetting(t, ctx, uid, gNoCategory, "")
	// gSoftDeletedCat: row points at status=2 category → Stage 1 INNER JOIN drops (H1).
	seedGroupSetting(t, ctx, uid, gSoftDeletedCat, catSoftDeleted)
	seedGroupSetting(t, ctx, uid, gExternal, catLive)
	// gFake: no group_setting row at all.

	// --- wire production objects ---
	convext.InitGlobalConvExtService(ctx)
	svc := convext.GetGlobalConvExtService()
	require.NotNil(t, svc, "ConvExt service must be initialized")
	checker := newThreadAuthChecker(ctx)
	svc.SetChannelAuthChecker(checker)
	svc.SetDefaultFollowedGroupGuard(&defaultFollowedGroupGuard{
		db:          newGroupCategoryDB(ctx),
		channelAuth: checker,
	})

	// --- exercise the production chain ---
	candidates := []string{
		gOK, gDisband, gWrongSpace, gNotMember, gNoCategory,
		gSoftDeletedCat, gExternal, gFake,
	}
	require.NoError(t, svc.AuthorizeAndMaterializeDefaultFollowedGroups(
		uid, spaceA, candidates),
		"production guard call must succeed (only Stage 1/2 infra errors propagate; rejections are silent drops)")

	// --- assertions: each candidate's ext-row state ---
	db := convext.NewDB(ctx)

	got := func(groupNo string) *convext.Model {
		t.Helper()
		m, err := db.Get(uid, spaceA, 2 /* Group */, groupNo)
		require.NoError(t, err)
		return m
	}

	t.Run("accepts member+spaceA+category", func(t *testing.T) {
		m := got(gOK)
		require.NotNil(t, m, "g-ok must be materialized")
		assert.Equal(t, int8(1), m.AutoFollowThreads,
			"materialized row must have auto_follow_threads=1")
		assert.Equal(t, int8(0), m.GroupUnfollowed)
	})

	t.Run("rejects Disband group", func(t *testing.T) {
		assert.Nil(t, got(gDisband),
			"Disband group must be dropped at Stage 2 (checkChannelAccess "+
				"explicitly rejects GroupStatusDisband)")
	})

	t.Run("rejects wrong-Space group (issue #151 review #2)", func(t *testing.T) {
		assert.Nil(t, got(gWrongSpace),
			"group whose space_id != request spaceID must be dropped at Stage 2 — "+
				"prevents cross-Space metadata leak when group_setting row "+
				"persists from another Space")
	})

	t.Run("rejects non-member", func(t *testing.T) {
		assert.Nil(t, got(gNotMember),
			"non-member group must be dropped at Stage 2 (ExistMember=false) — "+
				"prevents materialization for groups the user cannot otherwise see")
	})

	t.Run("rejects group with NULL category_id (Stage 1)", func(t *testing.T) {
		assert.Nil(t, got(gNoCategory),
			"group with group_setting.category_id IS NULL must be dropped at "+
				"Stage 1 — not a default-followed group")
	})

	t.Run("rejects group pointing at soft-deleted category (re-review H1)", func(t *testing.T) {
		assert.Nil(t, got(gSoftDeletedCat),
			"group whose group_setting.category_id references a status=2 category "+
				"must be dropped at Stage 1 — sidebar's LEFT JOIN would treat the "+
				"category as missing, so materializing here would create a phantom "+
				"auto_follow_threads=1 row for a group the user's sidebar never shows")
	})

	t.Run("accepts external-member group with sourceSpaceID match", func(t *testing.T) {
		m := got(gExternal)
		require.NotNil(t, m,
			"external-member group with sourceSpaceID == request spaceID must "+
				"survive Stage 2 — the external-group fallback in checkChannelAccess "+
				"is part of the legitimate visibility surface")
		assert.Equal(t, int8(1), m.AutoFollowThreads)
	})

	t.Run("rejects attacker-injected unknown group_no", func(t *testing.T) {
		assert.Nil(t, got(gFake),
			"group_no with no rows in group/group_member/group_setting must be "+
				"dropped at Stage 1 — issue #151 review #1's primary attack vector")
	})
}
