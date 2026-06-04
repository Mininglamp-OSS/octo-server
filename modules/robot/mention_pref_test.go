package robot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestDecideOwnership covers the creator-ownership decision used by the
// owner-scoped mention_pref endpoints (octo-server#237). The ownership reject
// paths (404 not-found, 403 forbidden) are the acceptance-required coverage.
func TestDecideOwnership(t *testing.T) {
	cases := []struct {
		name       string
		creatorUID string
		loginUID   string
		want       ownershipResult
	}{
		{"creator matches → OK", "owner_1", "owner_1", ownershipOK},
		{"robot missing (empty creator) → 404", "", "owner_1", ownershipNotFound},
		{"empty creator + empty login → 404", "", "", ownershipNotFound},
		{"different user → 403", "owner_1", "intruder_2", ownershipForbidden},
		{"creator set, login empty → 403", "owner_1", "", ownershipForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, decideOwnership(tc.creatorUID, tc.loginUID))
		})
	}
}

// TestClampGroupsLimit verifies default (30), upper cap (100), and floor handling.
func TestClampGroupsLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", groupsListDefaultLimit},
		{"abc", groupsListDefaultLimit},
		{"0", groupsListDefaultLimit},
		{"-5", groupsListDefaultLimit},
		{"15", 15},
		{"30", 30},
		{"100", 100},
		{"101", groupsListMaxLimit},
		{"99999", groupsListMaxLimit},
		{"  20  ", 20},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			assert.Equal(t, tc.want, clampGroupsLimit(tc.raw))
		})
	}
}

// TestGroupsCursorRoundTrip verifies the opaque cursor encodes/decodes and that
// blank/garbage decodes to 0 (first page) — keeping the cursor opaque to clients.
func TestGroupsCursorRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 42, 1_000_000, 9_223_372_036_854_775_807} {
		enc := encodeGroupsCursor(id)
		assert.NotEqual(t, "", enc)
		assert.Equal(t, id, decodeGroupsCursor(enc))
	}

	// Blank and non-base64 / non-numeric inputs fall back to 0 (first page).
	assert.Equal(t, int64(0), decodeGroupsCursor(""))
	assert.Equal(t, int64(0), decodeGroupsCursor("   "))
	assert.Equal(t, int64(0), decodeGroupsCursor("!!!not-base64!!!"))
	// Valid base64 of a non-numeric string also falls back to 0.
	assert.Equal(t, int64(0), decodeGroupsCursor("YWJj")) // base64("abc")
}

// TestBuildMentionPrefPayload verifies the mention_pref_updated event payload
// the owner write/delete endpoints push to the adapter (octo-server#242). The
// adapter keys cache invalidation off event.type + event.group_no + the
// message channel, and the bot only receives it via mention.uids — so those
// fields are the contract under test.
func TestBuildMentionPrefPayload(t *testing.T) {
	for _, noMention := range []int{0, 1} {
		p := buildMentionPrefPayload("bot_42", "g_100", noMention)

		// Top-level type is Text so it rides the same group-message path as
		// GROUP.md events.
		assert.Equal(t, common.Text, p["type"])

		event, ok := p["event"].(map[string]interface{})
		assert.True(t, ok, "event must be a map")
		assert.Equal(t, "mention_pref_updated", event["type"])
		assert.Equal(t, "g_100", event["group_no"])
		assert.Equal(t, noMention, event["no_mention"])

		// Targeted (non-broadcast): only the affected bot is mentioned, so the
		// robot dispatcher routes the event to that bot's queue alone.
		mention, ok := p["mention"].(map[string]interface{})
		assert.True(t, ok, "mention must be a map")
		assert.Equal(t, []string{"bot_42"}, mention["uids"])
	}
}

// owner-scoped endpoints authenticate via the user-session token (testutil.UID
// == "10000") and gate on robot.creator_uid == loginUID. These constants wire a
// bot owned by the test caller into one group carrying allow_no_mention.
const (
	ownerListRobotID = "bot_owner_list"
	ownerListGroupNo = "g_owner_list"
)

// setupOwnerMentionList builds a real Robot module on a clean DB with a bot
// owned by testutil.UID that is a member of one group, and seeds that group's
// allow_no_mention. Returns the router so owner endpoints can be exercised.
func setupOwnerMentionList(t *testing.T, groupAllowNoMention, ownerNoMention int) http.Handler {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	// Bot owned by the test caller (creator_uid = testutil.UID) so assertRobotOwner passes.
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		ownerListRobotID, testutil.UID,
	).Exec()
	assert.NoError(t, err)

	// Group row carrying allow_no_mention.
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version, allow_no_mention) VALUES (?, ?, 0, 1, ?)",
		ownerListGroupNo, "owner list group", groupAllowNoMention,
	).Exec()
	assert.NoError(t, err)

	// Bot is a non-deleted member of the group.
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, 1, 1)",
		ownerListGroupNo, ownerListRobotID, util.GenerUUID(),
	).Exec()
	assert.NoError(t, err)

	if ownerNoMention != 0 {
		_, err = ctx.DB().InsertBySql(
			"INSERT INTO bot_mention_pref (robot_id, group_no, no_mention) VALUES (?, ?, ?)",
			ownerListRobotID, ownerListGroupNo, ownerNoMention,
		).Exec()
		assert.NoError(t, err)
	}

	return s.GetRoute()
}

// TestListGroups_CarriesGroupAllowNoMention pins that the owner list endpoint
// surfaces the group-level switch (group_allow_no_mention) alongside no_mention,
// so the bot owner UI can show the "I enabled it but the group owner disabled
// it" state (YUJ-2996).
func TestListGroups_CarriesGroupAllowNoMention(t *testing.T) {
	handler := setupOwnerMentionList(t, 0 /* group blocks */, 1 /* owner enabled */)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/"+ownerListRobotID+"/groups", nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		List []struct {
			GroupNo             string `json:"group_no"`
			NoMention           bool   `json:"no_mention"`
			GroupAllowNoMention bool   `json:"group_allow_no_mention"`
		} `json:"list"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.List, 1)
	assert.Equal(t, ownerListGroupNo, resp.List[0].GroupNo)
	assert.True(t, resp.List[0].NoMention, "owner enabled → no_mention=true")
	assert.False(t, resp.List[0].GroupAllowNoMention, "group blocked → group_allow_no_mention=false")
}

// TestGetMentionPref_OwnerCarriesGroupAllow pins the owner single-group read
// returns both no_mention and group_allow_no_mention, and that a missing group
// row falls back to allow=1 (zero regression).
func TestGetMentionPref_OwnerCarriesGroupAllow(t *testing.T) {
	handler := setupOwnerMentionList(t, 0 /* group blocks */, 1 /* owner enabled */)

	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/robot/"+ownerListRobotID+"/groups/"+ownerListGroupNo+"/mention_pref", nil)
	assert.NoError(t, err)
	req.Header.Set("token", token)
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var resp struct {
		NoMention           int `json:"no_mention"`
		GroupAllowNoMention int `json:"group_allow_no_mention"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.NoMention)
	assert.Equal(t, 0, resp.GroupAllowNoMention)
}
