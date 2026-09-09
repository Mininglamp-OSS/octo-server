package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	aiteampkg "github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/stretchr/testify/assert"
)

// TestComputeEffectiveNoMention pins the two-axis AND truth table (YUJ-2996):
// the bot answers without an @mention only when BOTH the bot owner's intent
// (no_mention) and the group-level switch (group_allow_no_mention) are 1.
func TestComputeEffectiveNoMention(t *testing.T) {
	cases := []struct {
		noMention  int
		groupAllow int
		want       bool
	}{
		{0, 0, false},
		{0, 1, false},
		{1, 0, false},
		{1, 1, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, computeEffectiveNoMention(tc.noMention, tc.groupAllow),
			"no_mention=%d group_allow=%d", tc.noMention, tc.groupAllow)
	}
}

const (
	mpRobotID  = "bot_mp_1"
	mpBotToken = "bf_mp_token_1"
	mpGroupNo  = "g_mp_1"
)

// setupBotMentionPref wires a real BotAPI on a clean DB with one active bot
// (bot_token auth) that is a member of one group. Owner intent and group switch
// are seeded per-test so getMentionPref's two-axis AND can be asserted end-to-end.
func setupBotMentionPref(t *testing.T) (http.Handler, *config.Context) {
	t.Helper()
	s, ctx := testutil.NewTestServer()
	assert.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid, bot_token) VALUES (?, 1, ?, ?)",
		mpRobotID, "owner_mp", mpBotToken,
	).Exec()
	assert.NoError(t, err)

	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, vercode, is_deleted, status, version) VALUES (?, ?, ?, 0, 1, 1)",
		mpGroupNo, mpRobotID, util.GenerUUID(),
	).Exec()
	assert.NoError(t, err)

	return s.GetRoute(), ctx
}

// seedOwnerNoMention upserts the bot owner's per-group no_mention intent.
func seedOwnerNoMention(t *testing.T, ctx *config.Context, noMention int) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO bot_mention_pref (robot_id, group_no, no_mention) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE no_mention=VALUES(no_mention)",
		mpRobotID, mpGroupNo, noMention,
	).Exec()
	assert.NoError(t, err)
}

// seedGroupRow inserts the group row carrying allow_no_mention. Omitting it lets
// the handler exercise the no-row → default-1 fallback.
func seedGroupRow(t *testing.T, ctx *config.Context, allowNoMention int) {
	t.Helper()
	seedGroupRowWithPurpose(t, ctx, allowNoMention, "")
}

// seedGroupRowWithPurpose is seedGroupRow plus an explicit `purpose`, so a test
// can stand up an AI session container (purpose=aiteam.GroupPurpose) instead of
// an ordinary group.
func seedGroupRowWithPurpose(t *testing.T, ctx *config.Context, allowNoMention int, purpose string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, version, allow_no_mention, purpose) VALUES (?, ?, 0, 1, ?, ?)",
		mpGroupNo, "mp group", allowNoMention, purpose,
	).Exec()
	assert.NoError(t, err)
}

func getBotMentionPref(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/bot/groups/"+mpGroupNo+"/mention_pref", nil)
	assert.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+mpBotToken)
	handler.ServeHTTP(w, req)
	return w
}

type mentionPrefBody struct {
	NoMention           int  `json:"no_mention"`
	GroupAllowNoMention int  `json:"group_allow_no_mention"`
	Effective           bool `json:"effective"`
}

func decodeMentionPref(t *testing.T, w *httptest.ResponseRecorder) mentionPrefBody {
	t.Helper()
	assert.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var b mentionPrefBody
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b
}

// TestGetMentionPref_TruthTable exercises the four (no_mention, allow_no_mention)
// combinations end-to-end through the HTTP handler against a real DB. On the
// adapter-facing endpoint the `no_mention` field carries the AND-combined final
// decision (YUJ-2996 Blocking 1, option A) — i.e. no_mention == effective — so a
// legacy adapter reading only no_mention still obeys the group switch.
func TestGetMentionPref_TruthTable(t *testing.T) {
	cases := []struct {
		name          string
		noMention     int
		groupAllow    int
		wantEffective bool
	}{
		{"owner off, group allow → not effective", 0, 1, false},
		{"owner on, group allow → effective", 1, 1, true},
		{"owner on, group block → not effective", 1, 0, false},
		{"owner off, group block → not effective", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, ctx := setupBotMentionPref(t)
			seedOwnerNoMention(t, ctx, tc.noMention)
			seedGroupRow(t, ctx, tc.groupAllow)

			b := decodeMentionPref(t, getBotMentionPref(t, handler))
			// adapter-facing no_mention == effective (AND result), NOT raw intent.
			wantNoMention := 0
			if tc.wantEffective {
				wantNoMention = 1
			}
			assert.Equal(t, wantNoMention, b.NoMention, "adapter no_mention must equal effective")
			assert.Equal(t, tc.groupAllow, b.GroupAllowNoMention)
			assert.Equal(t, tc.wantEffective, b.Effective)
			assert.Equal(t, b.Effective, b.NoMention == 1, "no_mention and effective must agree")
		})
	}
}

// TestGetMentionPref_NoGroupRowDefaultsAllow pins the zero-regression fallback:
// when no group row exists, group_allow_no_mention defaults to 1, so an owner
// who has enabled no_mention still gets effective=true.
func TestGetMentionPref_NoGroupRowDefaultsAllow(t *testing.T) {
	handler, ctx := setupBotMentionPref(t)
	seedOwnerNoMention(t, ctx, 1)
	// Intentionally do NOT seed a group row.

	b := decodeMentionPref(t, getBotMentionPref(t, handler))
	assert.Equal(t, 1, b.NoMention)
	assert.Equal(t, 1, b.GroupAllowNoMention, "missing group row must fall back to allow=1")
	assert.True(t, b.Effective)
}

// TestGetMentionPref_NoOwnerRecordDefaultsOff pins that a bot with no
// bot_mention_pref record defaults to no_mention=0 (account-level default), so
// even an allowing group does not make it effective.
func TestGetMentionPref_NoOwnerRecordDefaultsOff(t *testing.T) {
	handler, ctx := setupBotMentionPref(t)
	seedGroupRow(t, ctx, 1)
	// Intentionally do NOT seed a bot_mention_pref record.

	b := decodeMentionPref(t, getBotMentionPref(t, handler))
	assert.Equal(t, 0, b.NoMention)
	assert.Equal(t, 1, b.GroupAllowNoMention)
	assert.False(t, b.Effective)
}

// TestResolveEffectiveNoMention pins the full decision, including the AI session
// container override. The container cases are the load-bearing ones: a container
// is effective even when BOTH owner axes say no, because in a container the @ is
// not a routing signal — delivery is resolved from ai_team_agent, so requiring an
// @ would only make the session look broken.
func TestResolveEffectiveNoMention(t *testing.T) {
	cases := []struct {
		name       string
		noMention  int
		groupAllow int
		purpose    string
		want       bool
	}{
		// Ordinary groups: unchanged two-axis AND.
		{"ordinary 0/0", 0, 0, "", false},
		{"ordinary 0/1", 0, 1, "", false},
		{"ordinary 1/0", 1, 0, "", false},
		{"ordinary 1/1", 1, 1, "", true},
		// AI session container: server-decided, both axes irrelevant.
		{"container 0/0", 0, 0, aiteampkg.GroupPurpose, true},
		{"container 0/1", 0, 1, aiteampkg.GroupPurpose, true},
		{"container 1/0", 1, 0, aiteampkg.GroupPurpose, true},
		{"container 1/1", 1, 1, aiteampkg.GroupPurpose, true},
		// An unrelated purpose must NOT take the container branch.
		{"other purpose 0/1", 0, 1, "something_else", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				resolveEffectiveNoMention(tc.noMention, tc.groupAllow, tc.purpose))
		})
	}
}

// TestGetMentionPref_AIContainerNeedsNoOwnerRecord is the regression this change
// exists for. An AI session container has NO bot_mention_pref row and can never
// get one (the owner endpoints reject writes for these groups), so before this
// change the endpoint answered effective=false and the adapter's mention gate
// dropped every non-@ message in the session into history — the AI never replied.
func TestGetMentionPref_AIContainerNeedsNoOwnerRecord(t *testing.T) {
	handler, ctx := setupBotMentionPref(t)
	seedGroupRowWithPurpose(t, ctx, 1, aiteampkg.GroupPurpose)
	// Intentionally do NOT seed a bot_mention_pref record — a container never has one.

	b := decodeMentionPref(t, getBotMentionPref(t, handler))
	assert.True(t, b.Effective, "AI session container must be 免@ without any owner record")
	assert.Equal(t, 1, b.NoMention, "legacy adapters reading only no_mention must also see 免@")
	assert.Equal(t, 1, b.GroupAllowNoMention)
}

// TestGetMentionPref_AIContainerIgnoresBlockedGroupSwitch pins that the container
// override outranks a blocking allow_no_mention, and that the response still
// reports the RAW column value. Containers are created with allow_no_mention=1
// and the group-setting endpoints reject these groups, so a 0 can only come from
// a hand-edited row — in which case a debugger needs to see the 0, while the
// adapter (which gates on `effective` alone) keeps answering.
func TestGetMentionPref_AIContainerIgnoresBlockedGroupSwitch(t *testing.T) {
	handler, ctx := setupBotMentionPref(t)
	seedGroupRowWithPurpose(t, ctx, 0, aiteampkg.GroupPurpose)
	seedOwnerNoMention(t, ctx, 0)

	b := decodeMentionPref(t, getBotMentionPref(t, handler))
	assert.True(t, b.Effective, "container override must outrank the group switch")
	assert.Equal(t, 1, b.NoMention)
	assert.Equal(t, 0, b.GroupAllowNoMention, "group_allow_no_mention stays the raw column value")
}

// TestGetMentionPref_OrdinaryGroupUnaffectedByPurposeColumn guards the blast
// radius: reading the new column must not change any ordinary group's answer,
// including a group whose purpose is some other non-empty value.
func TestGetMentionPref_OrdinaryGroupUnaffectedByPurposeColumn(t *testing.T) {
	handler, ctx := setupBotMentionPref(t)
	seedGroupRowWithPurpose(t, ctx, 1, "not_an_ai_container")
	seedOwnerNoMention(t, ctx, 0)

	b := decodeMentionPref(t, getBotMentionPref(t, handler))
	assert.False(t, b.Effective, "a non-container purpose must not unlock 免@")
	assert.Equal(t, 0, b.NoMention)
	assert.Equal(t, 1, b.GroupAllowNoMention)
}
