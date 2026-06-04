package group

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestGroup_AllowNoMention_DefaultsTo1 pins the zero-regression guarantee: a
// group row inserted WITHOUT allow_no_mention takes the DB column default 1
// (allow). This mirrors how the ALTER TABLE migration backfills existing rows,
// so deploying this feature does not silently turn off any existing no-@ bot.
func TestGroup_AllowNoMention_DefaultsTo1(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	groupNo := "g-default-nomention"
	// Insert via raw SQL omitting allow_no_mention so the column default applies.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version) VALUES (?, ?, 0, 1)",
		groupNo, "default nomention",
	).Exec()
	assert.NoError(t, err)

	var allow int
	err = ctx.DB().Select("allow_no_mention").From("`group`").
		Where("group_no=?", groupNo).LoadOne(&allow)
	assert.NoError(t, err)
	assert.Equal(t, 1, allow, "省略列时 allow_no_mention 应取 DB 默认 1（零回归）")
}

// TestGroup_AllowNoMention_UpdateRoundTrips pins that the db.Update column
// mapping persists allow_no_mention both ways (the handler's updateGroup path).
func TestGroup_AllowNoMention_UpdateRoundTrips(t *testing.T) {
	f, _ := setupBotOwnershipGroup(t)

	g, err := f.db.QueryWithGroupNo("g_bot_own")
	assert.NoError(t, err)

	g.AllowNoMention = 0
	assert.NoError(t, f.db.Update(g))
	g, err = f.db.QueryWithGroupNo("g_bot_own")
	assert.NoError(t, err)
	assert.Equal(t, 0, g.AllowNoMention, "Update 应把 allow_no_mention=0 写回")

	g.AllowNoMention = 1
	assert.NoError(t, f.db.Update(g))
	g, err = f.db.QueryWithGroupNo("g_bot_own")
	assert.NoError(t, err)
	assert.Equal(t, 1, g.AllowNoMention, "Update 应把 allow_no_mention=1 写回")
}

// TestGroupSettingUpdate_AllowNoMentionRangeIsRequestInvalid pins that an
// out-of-range allow_no_mention value is a 400 client validation error, not the
// store_failed (500). The caller is the creator so the range check (which runs
// before any DB/event write) is what rejects.
func TestGroupSettingUpdate_AllowNoMentionRangeIsRequestInvalid(t *testing.T) {
	_, h := setupBotOwnershipGroup(t)

	w := putGroupSetting(t, h, "g_bot_own", `{"allow_no_mention":2}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
		"allow_no_mention 越界应是 400 校验错误而非内部错误, body=%s", w.Body.String())
}

// TestGroupSettingUpdate_AllowNoMention_NonManagerForbidden pins that a
// non-manager/creator toggling the group-level switch gets 403, not 500. The
// permission check runs before any DB/event write.
func TestGroupSettingUpdate_AllowNoMention_NonManagerForbidden(t *testing.T) {
	s, ctx := testutil.NewTestServer()
	wireI18nRendererForGroupTest(s)
	f := New(ctx)

	assert.NoError(t, testutil.CleanAllTables(ctx))

	groupNo := "g-nomention-deny"
	err := f.db.Insert(&Model{GroupNo: groupNo, Name: "nomention deny", Creator: "other-owner", Status: GroupStatusNormal, Version: 1})
	assert.NoError(t, err)

	w := putGroupSetting(t, s.GetRoute(), groupNo, `{"allow_no_mention":0}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.creator_or_manager_only",
		"非管理员改群级免@开关应是 403 而非内部错误, body=%s", w.Body.String())
}
