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

// TestGroupSettingUpdate_AllowNoMention_FractionalRejected pins YUJ-2996
// Blocking 2: a fractional JSON value must be rejected up front (400) instead of
// being truncated into a valid 0/1 and silently flipping the group switch.
// 0.9 → would have truncated to 0, 1.9 → to 1; both must now fail validation,
// alongside the plain out-of-range 2 / -1.
func TestGroupSettingUpdate_AllowNoMention_FractionalRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"0.9 truncates to 0", `{"allow_no_mention":0.9}`},
		{"1.9 truncates to 1", `{"allow_no_mention":1.9}`},
		{"2 out of range", `{"allow_no_mention":2}`},
		{"-1 out of range", `{"allow_no_mention":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, h := setupBotOwnershipGroup(t)

			// Seed a known switch state (1=allow) so a successful (buggy) write
			// would be observable as a flip to 0.
			g, err := f.db.QueryWithGroupNo("g_bot_own")
			assert.NoError(t, err)
			g.AllowNoMention = 1
			assert.NoError(t, f.db.Update(g))

			w := putGroupSetting(t, h, "g_bot_own", tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code, "wire status 固定 400, body=%s", w.Body.String())
			assert.Contains(t, w.Body.String(), "err.server.group.request_invalid",
				"非法 allow_no_mention 值应是 400 校验错误而非内部错误, body=%s", w.Body.String())

			// The group switch must remain unchanged after a rejected write.
			g, err = f.db.QueryWithGroupNo("g_bot_own")
			assert.NoError(t, err)
			assert.Equal(t, 1, g.AllowNoMention, "被拒的写入不得改动群开关, body=%s", w.Body.String())
		})
	}
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

// TestNewChannelResp_CarriesAllowNoMention pins YUJ-3153 Bug 2 root cause: the
// channel serialization (channels/{uid}/{type}, which feeds the web's
// channelInfo.orgData) must expose allow_no_mention. Before the fix the field
// was written to the DB but omitted from extraMap, so the client read undefined,
// defaulted to 1, and the switch was stuck "on" (refresh bounced it back).
func TestNewChannelResp_CarriesAllowNoMention(t *testing.T) {
	for _, want := range []int{0, 1} {
		resp := newChannelRespWithGroupResp(&GroupResp{
			GroupNo:        "g-extra",
			Name:           "extra group",
			AllowNoMention: want,
		})
		got, ok := resp.Extra["allow_no_mention"]
		assert.True(t, ok, "extra 必须透出 allow_no_mention，否则前端开关关不掉")
		assert.Equal(t, want, got, "extra.allow_no_mention 应等于群 model 的值")
	}
}

// TestGroupSettingUpdate_AllowNoMention_SilentToggleSucceeds pins YUJ-3153 Bug
// 3: toggling the switch as creator now goes through the silent
// SendChannelUpdateToGroup path (like mute / allow_view_history_msg) instead of
// commmitGroupUpdateEvent. The latter published a GroupUpdate + wkevent.Message
// that rendered as a blank announcement and an unread red dot on every click.
// A successful 200 + persisted value here proves the toggle no longer routes
// through the visible-message event path (which nil-derefs ctx.Event in testutil).
func TestGroupSettingUpdate_AllowNoMention_SilentToggleSucceeds(t *testing.T) {
	f, h := setupBotOwnershipGroup(t)

	g, err := f.db.QueryWithGroupNo("g_bot_own")
	assert.NoError(t, err)
	g.AllowNoMention = 1
	assert.NoError(t, f.db.Update(g))

	w := putGroupSetting(t, h, "g_bot_own", `{"allow_no_mention":0}`)
	assert.Equal(t, http.StatusOK, w.Code, "静默开关切换应成功返回 200, body=%s", w.Body.String())

	g, err = f.db.QueryWithGroupNo("g_bot_own")
	assert.NoError(t, err)
	assert.Equal(t, 0, g.AllowNoMention, "切换后 allow_no_mention 应持久化为 0")
}

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
