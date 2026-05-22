// Package bot_api · PR#121 R5 regression guards (Jerry-Xin + lml2468,
// 2026-05-22).
//
// Three critical regressions surfaced by the R5 review of the rebased
// PR#121 branch. This file pins the two regressions whose fixes have a
// natural unit-test surface in the fan-out / send paths:
//
//   - B1. findActiveGrantsForChannelByGrantors ignored explicit
//     `obo_scopes` rows with `enabled=0`. A channel admin could
//     explicitly disable a channel and a peer could still trigger
//     fan-out by @-mentioning the grantor — completely defeating the
//     admin's disable. The fix adds an anti-join against obo_scopes
//     for the `enabled=0` row in both the SQL and the in-memory fake;
//     this test reproduces the original repro and locks in the new
//     "explicit disabled scope row takes precedence" invariant.
//   - B3. botHasActiveGrantFrom (the YUJ-1418 grantor-reply bypass
//     auth helper) called findActiveGrantByGrantorBot, which requires
//     `active=1 AND global_enabled=1`, so the bypass fell through to
//     the strict OBO scope check the moment a user paused the
//     persona (`global_enabled=0`) — breaking direct grantor→bot
//     DM conversation. The fix routes the bypass through
//     findGrantByGrantorBotActiveOnly (active=1 only). The
//     TestSendMessage_OBO_GrantorReplyBypass_DM_GlobalDisabled
//     test that pinned this lives in obo_send_test.go.
//
// B2 (atomic create-or-reactivate) and W1 (oboUpdateGrant active gate)
// are covered respectively by the v2 mutex test surface in
// obo_v2_test.go (still passes through the restored
// createOrReactivateGrantAtomic path) and TestOBOUpdate_RejectsRevoked
// in this file.
package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// TestFanout_PR121R5_B1_GrantorMentionDisabledScope_NoFanOut — repros
// the B1 blocker: a channel that has an explicit `obo_scopes` row with
// `enabled=0` must NOT fan out when an inbound message @-mentions the
// grantor. The mention path's UID-filtered query
// (findActiveGrantsForChannelByGrantors) previously ignored
// `obo_scopes` entirely, so admin's intent to disable the channel was
// silently overridden by any peer @-mentioning the grantor.
//
// Post-fix: the LEFT JOIN anti-join against obo_scopes filters the
// grant out of the @-mention path, and the implicit-scope feeder
// (findGlobalGrantsWithoutScope) also excludes the channel because a
// scope row exists. Net result: zero fan-out copies.
func TestFanout_PR121R5_B1_GrantorMentionDisabledScope_NoFanOut(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)

	// Find the seeded grant ID so we can install the disabled scope
	// row against the correct grant.
	gid := int64(0)
	for id, g := range s.grants {
		if g != nil && g.GrantorUID == tGrantor && g.GranteeBotUID == tBot {
			gid = id
			break
		}
	}
	if gid == 0 {
		t.Fatalf("seeded grant not found in fake store")
	}

	// Admin explicitly disables the channel for this grant
	// (POST /v1/obo/scopes equivalent — enabled=0).
	if _, err := s.insertScope(gid, ch, ct, 0); err != nil {
		t.Fatalf("insertScope(enabled=0): %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		// Peer @-mentions the grantor — pre-fix this would have
		// triggered a fan-out copy despite the admin's disable.
		Payload: []byte(`{"type":1,"content":"@yu help","mention":{"uids":["` +
			tGrantor + `"]}}`),
	}

	if got := ba.fanoutForMessage(msg); got != 0 {
		t.Fatalf("B1 regression: @grantor in a channel with explicit enabled=0 scope row must NOT fan out, got %d copies", got)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("B1 regression: dispatch must not be invoked, got %d captured req(s)", len(fc.copies))
	}
}

// TestFanout_PR121R5_B1_GrantorMentionEnabledScope_StillFansOut — guard
// against the over-correction risk: a scope row with `enabled=1` must
// still allow fan-out via @-mention. This isolates the anti-join from
// any accidental "JOIN obo_scopes -> require enabled=1" mis-fix that
// would re-break the YUJ-1538 "global_enabled-only, no scope row"
// implicit-scope semantics.
func TestFanout_PR121R5_B1_GrantorMentionEnabledScope_StillFansOut(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantNoScope(t)

	gid := int64(0)
	for id, g := range s.grants {
		if g != nil && g.GrantorUID == tGrantor && g.GranteeBotUID == tBot {
			gid = id
			break
		}
	}
	if gid == 0 {
		t.Fatalf("seeded grant not found in fake store")
	}
	if _, err := s.insertScope(gid, ch, ct, 1); err != nil {
		t.Fatalf("insertScope(enabled=1): %v", err)
	}

	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload: []byte(`{"type":1,"content":"@yu help","mention":{"uids":["` +
			tGrantor + `"]}}`),
	}
	if got := ba.fanoutForMessage(msg); got != 1 {
		t.Fatalf("@grantor with explicit enabled=1 scope row must fan out, got %d copies", got)
	}
}

// TestOBOUpdate_PR121R5_W1_RejectsRevokedGrant — W1 regression guard.
// PR#121 rebase dropped the YUJ-1424 `grant.Active != 1 → 404` check
// at the top of oboUpdateGrant, allowing a caller to PUT mode /
// global_enabled / persona_prompt on a tombstoned grant. The row would
// still have active=0 (so it cannot trigger fan-out) but a follow-up
// findGrantByID read-back surfaces it as "live" mode + global_enabled
// to the UI, producing misleading client state.
//
// Post-fix: the active=1 gate fires before BindJSON; revoked grants
// reject with 404 (matching requireOwnedGrant's existence-leak
// posture). Callers that want to revive a revoked grant must POST
// /v1/obo/grants (the atomic create-or-reactivate flow handles the
// reactivation in-place).
func TestOBOUpdate_PR121R5_W1_RejectsRevokedGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botID   = "bot_clone_001"
		grantor = "user_admin"
	)

	s := newFakeOBOStore()
	s.seedBot(botID, grantor)
	grant, _, err := s.createOrReactivateGrantAtomic(grantor, botID, "auto", "")
	if err != nil {
		t.Fatalf("createOrReactivateGrantAtomic: %v", err)
	}
	// Tombstone the grant so the next PUT hits the revoked path.
	if err := s.revokeGrant(grant.ID); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-w1-update-revoked"),
		oboStoreOverride: s,
	}

	enabled := 1
	body, _ := json.Marshal(oboUpdateGrantReq{GlobalEnabled: &enabled})
	httpReq := httptest.NewRequest(http.MethodPut,
		"/v1/obo/grants/"+itoa(grant.ID), bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	gc.Params = gin.Params{{Key: "id", Value: itoa(grant.ID)}}
	c := &wkhttp.Context{Context: gc}
	c.Set("uid", grantor)

	ba.oboUpdateGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("W1 regression: PUT on a revoked grant must return 404, got status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	// Verify the row did not pick up the requested global_enabled flip.
	after, _ := s.findGrantByID(grant.ID)
	if after == nil {
		t.Fatalf("grant disappeared after rejected PUT")
	}
	if after.Active != 0 {
		t.Fatalf("rejected PUT must not flip active back on, got active=%d", after.Active)
	}
	if after.GlobalEnabled != 0 {
		t.Fatalf("rejected PUT must not mutate global_enabled, got %d", after.GlobalEnabled)
	}
}

// itoa is a tiny helper to keep the body of the W1 test focused on
// behavior rather than strconv noise.
func itoa(n int64) string {
	// Range is small for tests; sprint is fine but strconv is allocation-free.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
