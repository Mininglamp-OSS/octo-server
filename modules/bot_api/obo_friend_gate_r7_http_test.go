// Package bot_api · PR#82 R7 — HTTP-level regression for the "no
// bypass without OBO context" invariant.
//
// Jerry-Xin's R7 finding (2026-05-19 on head a07b372): the bypass in
// `isFriendOrOBOBypass` was unconditional, so a bot that held any
// active OBO grant covering a target could call sendMessage / typing /
// readReceipt / messages-sync WITHOUT `on_behalf_of` and dispatch as
// the bot itself — leaking the managed-persona bot as a direct contact
// to a user that had not opted in.
//
// These tests build the full HTTP handler stack (sendMessage / typing /
// readReceipt / syncMessages) with:
//   - a bot that is NOT friends with the peer (friendCheckOverride → false)
//   - an active OBO grant + scope covering (bot, peer)
//   - no `on_behalf_of` on the request
//
// and assert that the gate denies. Pair tests assert the SAME fixture
// with `on_behalf_of` set still passes the friend gate (the
// managed-persona happy path), so the gate is gated by the request
// context, not regressed across the board.
package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// newBAforR7Test wires a BotAPI with:
//   - fake OBO store with an enabled grant (admin, bot) scope=peer DM
//   - oboChannelAccessOverride allowing admin to reach peer
//   - friendCheckOverride returning false (bot NOT friends with peer)
//   - dispatchOverride capture (so we can assert dispatch DID/DID NOT fire)
//   - fakeSpaceQuerier so sendMessage's space_id resolve doesn't NPE
func newBAforR7Test() (*BotAPI, *dispatchCapture) {
	const (
		admin = "user_admin"
		bot   = "bot_clone_james"
		peer  = "u_bob"
	)
	_ = admin
	_ = bot
	_ = peer
	s := newFakeOBOStore()
	gid, _ := s.insertGrant("user_admin", "bot_clone_james", "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, "u_bob", common.ChannelTypePerson.Uint8(), 1)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-r7-http-test"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space_A"},
		dispatchOverride: dc.hook,
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return uid == "user_admin" && channelID == "u_bob", nil
		},
		friendCheckOverride: func(uid, toUID string) (bool, error) {
			return false, nil // bot NOT friends with peer
		},
	}
	return ba, dc
}

// buildBotCtx assembles a *wkhttp.Context for the given *httptest.Recorder
// and request body, with the BotKindUser auth markers + a non-creator
// robot (creator != peer) so the DM-creator bypass does NOT fire and
// the friend gate is forced to make the decision.
func buildBotCtx(rec *httptest.ResponseRecorder, body []byte, path string) *wkhttp.Context {
	httpReq := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, "bot_clone_james")
	c.Set(CtxKeyBotKind, BotKindUser)
	// Creator != peer (peer = "u_bob"; creator = "user_admin") so the
	// "isCreator" short-circuit in checkSendPermission / syncMessages
	// does NOT fire and the friend gate runs.
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_clone_james", CreatorUID: "user_admin"})
	return c
}

// TestBotAPI_FriendGate_NoBypassWithoutOBOContext — PR#82 R7 headline
// regression for sendMessage. Bot not friends with peer, active grant
// covers (bot, peer), but `on_behalf_of` is omitted → sendMessage MUST
// deny with "bot is not a friend of this user" AND dispatch MUST NOT
// fire (no leakage past the gate).
func TestBotAPI_FriendGate_NoBypassWithoutOBOContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ba, dc := newBAforR7Test()

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   "u_bob",
		ChannelType: common.ChannelTypePerson.Uint8(),
		// NO OnBehalfOf — this is the regression scenario.
		Payload: map[string]interface{}{"content": "direct bot→bob send", "type": 1},
	})
	rec := httptest.NewRecorder()
	c := buildBotCtx(rec, body, "/v1/bot/sendMessage")
	ba.sendMessage(c)

	if !strings.Contains(rec.Body.String(), "bot is not a friend of this user") {
		t.Fatalf("PR#82 R7: sendMessage WITHOUT on_behalf_of must hit the friend-gate deny path even when an OBO grant covers the peer; got body=%s", rec.Body.String())
	}
	if dc.captured != nil {
		t.Fatalf("PR#82 R7: dispatch must NOT fire when the friend gate denies; got %+v", dc.captured)
	}
}

// TestBotAPI_FriendGate_NoBypassWithoutOBOContext_Typing — same
// invariant for the typing path. Typing has no on_behalf_of field; the
// bypass MUST never apply.
func TestBotAPI_FriendGate_NoBypassWithoutOBOContext_Typing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ba, _ := newBAforR7Test()

	body, _ := json.Marshal(BotTypingReq{
		ChannelID:   "u_bob",
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	rec := httptest.NewRecorder()
	c := buildBotCtx(rec, body, "/v1/bot/typing")
	ba.typing(c)

	if !strings.Contains(rec.Body.String(), "bot is not a friend of this user") {
		t.Fatalf("PR#82 R7: typing must hit the friend-gate deny path when bot↔peer not friends, even with OBO grant; got body=%s", rec.Body.String())
	}
}

// TestBotAPI_FriendGate_NoBypassWithoutOBOContext_ReadReceipt — same
// invariant for readReceipt.
func TestBotAPI_FriendGate_NoBypassWithoutOBOContext_ReadReceipt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ba, _ := newBAforR7Test()

	body, _ := json.Marshal(BotReadReceiptReq{
		ChannelID:   "u_bob",
		ChannelType: common.ChannelTypePerson.Uint8(),
	})
	rec := httptest.NewRecorder()
	c := buildBotCtx(rec, body, "/v1/bot/readReceipt")
	ba.readReceipt(c)

	if !strings.Contains(rec.Body.String(), "bot is not a friend of this user") {
		t.Fatalf("PR#82 R7: readReceipt must hit the friend-gate deny path when bot↔peer not friends, even with OBO grant; got body=%s", rec.Body.String())
	}
}

// TestBotAPI_FriendGate_NoBypassWithoutOBOContext_SyncMessages — same
// invariant for messages/sync. messages/sync has no on_behalf_of
// field; the bypass MUST never apply (the sync response stream goes
// back to the bot, not proxied as the grantor).
func TestBotAPI_FriendGate_NoBypassWithoutOBOContext_SyncMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ba, _ := newBAforR7Test()

	body, _ := json.Marshal(BotSyncMessagesReq{
		ChannelID:   "u_bob",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Limit:       50,
	})
	rec := httptest.NewRecorder()
	c := buildBotCtx(rec, body, "/v1/bot/messages/sync")
	ba.syncMessages(c)

	if !strings.Contains(rec.Body.String(), "bot is not a friend of this user") {
		t.Fatalf("PR#82 R7: messages/sync must hit the friend-gate deny path when bot↔peer not friends, even with OBO grant; got body=%s", rec.Body.String())
	}
}

// TestBotAPI_FriendGate_BypassAppliesWhenOBOContextPresent_SendMessage —
// companion to the negative tests above: the SAME fixture but with
// `on_behalf_of` set MUST pass the friend gate (managed-persona happy
// path). This locks in that the only delta between deny and allow is
// the presence of validated OBO context on the request.
func TestBotAPI_FriendGate_BypassAppliesWhenOBOContextPresent_SendMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ba, dc := newBAforR7Test()

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   "u_bob",
		ChannelType: common.ChannelTypePerson.Uint8(),
		OnBehalfOf:  "user_admin", // managed-persona send as admin
		Payload:     map[string]interface{}{"content": "hi bob, it's me admin", "type": 1},
	})
	rec := httptest.NewRecorder()
	c := buildBotCtx(rec, body, "/v1/bot/sendMessage")
	ba.sendMessage(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("managed-persona send (with on_behalf_of) must pass the gate; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if dc.captured == nil {
		t.Fatal("managed-persona send must dispatch; dispatch capture empty")
	}
	if dc.captured.FromUID != "user_admin" {
		t.Errorf("OBO substitution: FromUID should be user_admin (grantor), got %q", dc.captured.FromUID)
	}
}
