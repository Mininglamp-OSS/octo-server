// Package bot_api · YUJ-1166 — Integration test for /v1/bot/sendMessage
// with the on_behalf_of field set.
//
// Exercises the full sendMessage handler with a stubbed oboStore + space
// querier + dispatch capture, then asserts:
//   - Authorized OBO sets FromUID = on_behalf_of (not robotID)
//   - Authorized OBO marks payload with __obo_processed__=true (gate 3
//     marker; PR#82 review #2 P1-2 — reserved-namespace key) and
//     actual_sender_uid=<bot>
//   - Unauthorized OBO returns 400 with the "obo not authorized" body
//     and does NOT dispatch (no leakage past the auth check)
//
// Reuses the existing dispatchCapture from send_test.go.
package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

func TestSendMessage_OBO_Authorized_SwapsFromUID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botID   = "bot_clone_001"
		grantor = "user_yu"
		group   = "group_42"
		authSp  = "space_A"
	)

	// Stub OBO: enabled grant + scope for (grantor, bot) in this group.
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(grantor, botID, "auto")
	enable := 1
	_ = s.updateGrant(gid, "", &enable)
	_, _ = s.insertScope(gid, group, common.ChannelTypeGroup.Uint8(), 1)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-obo-send-it"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: s,
		// PR#82 round-2 P1-A — checkOBO now re-checks live channel access.
		// Default to "allowed" for the happy-path send integration; tests
		// that need denial path use TestOBO_CheckOBO_GrantorMembershipRevoked_403.
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   group,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		OnBehalfOf:  grantor,
		Payload:     map[string]interface{}{"content": "hello as yu", "type": 1},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	// Creator = a group bot path — for ChannelTypeGroup the checkSendPermission
	// branch hits `group_member` DB; bypass that by using ChannelTypePerson
	// via the creator path. But we want a group test for fan-out coherence,
	// so set up minimal robot row + skip the DB lookup by short-circuiting
	// through `BotKindUser` with `ChannelType=Person` and channelID=creator.
	//
	// Re-route to PERSONAL DM (which has the creator-bypass path).
	body, _ = json.Marshal(BotSendMessageReq{
		ChannelID:   grantor, // DM peer == creator → bypasses friend check
		ChannelType: common.ChannelTypePerson.Uint8(),
		OnBehalfOf:  "user_alice", // different uid; we'll swap stub below
		Payload:     map[string]interface{}{"content": "hi alice", "type": 1},
	})

	// Switch the grant to (alice, bot) for a DM to peer=alice. Rebuild the
	// fake to keep the test self-contained.
	s2 := newFakeOBOStore()
	gid2, _ := s2.insertGrant("user_alice", botID, "auto")
	_ = s2.updateGrant(gid2, "", &enable)
	_, _ = s2.insertScope(gid2, grantor, common.ChannelTypePerson.Uint8(), 1)
	ba.oboStoreOverride = s2

	httpReq = httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	gc, _ = gin.CreateTestContext(rec)
	gc.Request = httpReq
	c = &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: grantor})

	ba.sendMessage(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if dc.captured == nil {
		t.Fatal("dispatch was not called")
	}
	if dc.captured.FromUID != "user_alice" {
		t.Errorf("FromUID should be on_behalf_of (user_alice), got %q", dc.captured.FromUID)
	}
	// Payload markers.
	var got map[string]interface{}
	if err := json.Unmarshal(dc.captured.Payload, &got); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if v, _ := got[oboProcessedMarkerKey].(bool); !v {
		t.Errorf("payload missing %s marker: %v", oboProcessedMarkerKey, got)
	}
	if got["actual_sender_uid"] != botID {
		t.Errorf("payload actual_sender_uid should be %q, got %v", botID, got["actual_sender_uid"])
	}
}

func TestSendMessage_OBO_Unauthorized_Returns400Body(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botID   = "bot_clone_001"
		grantor = "user_yu"
		group   = "group_42"
		authSp  = "space_A"
	)

	// Empty OBO store → no grant for anyone → unauthorized.
	s := newFakeOBOStore()
	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-obo-send-deny"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: s,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   grantor, // DM, creator-bypass
		ChannelType: common.ChannelTypePerson.Uint8(),
		OnBehalfOf:  grantor,
		Payload:     map[string]interface{}{"content": "denied", "type": 1},
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: grantor})

	ba.sendMessage(c)
	// ResponseError → 400 with body containing the message. Asserting on
	// the body rather than the code keeps the test independent of the
	// project's choice of error transport.
	if !strings.Contains(rec.Body.String(), ErrOBONotAuthorized.Error()) {
		t.Fatalf("expected obo-not-authorized in body, got %s", rec.Body.String())
	}
	if dc.captured != nil {
		t.Fatalf("dispatch must NOT be called when OBO denies; got %+v", dc.captured)
	}
}

// TestSendMessage_NoOBO_LegacyPath — sanity guard that adding the OBO
// branch did not change behavior when OnBehalfOf is empty: FromUID still
// = robotID and the obo_processed marker is NOT injected. This is the
// "old functionality not regressed" smoke check from RFC §10.1.
func TestSendMessage_NoOBO_LegacyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		botID  = "bot_legacy"
		owner  = "creator_uid"
		authSp = "space_A"
	)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-legacy"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: newFakeOBOStore(),
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   owner,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     map[string]interface{}{"content": "hi", "type": 1},
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: owner})

	ba.sendMessage(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if dc.captured == nil {
		t.Fatal("dispatch missing")
	}
	if dc.captured.FromUID != botID {
		t.Errorf("legacy path FromUID must = robotID, got %q", dc.captured.FromUID)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(dc.captured.Payload, &got)
	if _, has := got[oboProcessedMarkerKey]; has {
		t.Errorf("legacy path should not set %s marker, got %v", oboProcessedMarkerKey, got)
	}
}

// keep the compiler happy if msg-content imports go unused in a refactor
var _ = config.MsgSendReq{}

// TestSendMessage_RejectsReservedOBOKey — inbound /v1/bot/sendMessage
// payloads carrying any `__obo_*` top-level key are rejected before any
// other validation. This locks down gate 3's marker key
// (`__obo_processed__`) and any future server-only OBO field: a bot
// cannot forge or suppress them via the public REST API.
// PR#82 review #2 P1-2 regression guard.
func TestSendMessage_RejectsReservedOBOKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		botID  = "bot_legacy"
		owner  = "creator_uid"
		authSp = "space_A"
	)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-reject-reserved"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: newFakeOBOStore(),
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   owner,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":           "trying to bypass gate 3",
			"type":              1,
			"__obo_processed__": true, // <-- malicious / forbidden
		},
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: owner})

	ba.sendMessage(c)
	// Body must carry the reject message; dispatch must NOT fire.
	if !strings.Contains(rec.Body.String(), "__obo_") {
		t.Fatalf("expected reject body to mention __obo_ prefix, got %s", rec.Body.String())
	}
	if dc.captured != nil {
		t.Fatalf("dispatch must NOT fire when reserved OBO key is rejected, got %+v", dc.captured)
	}
}

// TestSendMessage_RejectsReservedOBOKey_OtherPrefix — covers an
// arbitrary `__obo_*` key (not just the marker). Ensures the validator
// is namespace-wide, so future server-only OBO fields cannot be spoofed
// by adding them in the bot client.
func TestSendMessage_RejectsReservedOBOKey_OtherPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		botID  = "bot_legacy"
		owner  = "creator_uid"
		authSp = "space_A"
	)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-reject-reserved-other"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: newFakeOBOStore(),
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   owner,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":               "hi",
			"type":                  1,
			"__obo_actual_sender__": "victim_bot",
		},
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: owner})

	ba.sendMessage(c)
	if dc.captured != nil {
		t.Fatalf("dispatch must NOT fire for any __obo_* key, got %+v", dc.captured)
	}
}

// TestBotMessage_OBOReservedKeysKept — PR#82 R8 contract guard.
// Asserts that the bot-API behavior on reserved `__obo_*` keys is
// UNCHANGED by the user-ingress strip fix: the bot ingress still
// REJECTS the request (vs the user ingress, which silently strips).
//
// Why both behaviors coexist
// ==========================
// The R8 fix added a silent strip at the user-message ingress
// (modules/message/api.go → m.sendMessage) so a normal user can't
// forge gate-3 markers. The bot ingress already rejected the same
// prefix and we MUST NOT relax that — bot authors are expected to
// know the reserved namespace, and a loud 4xx makes integration bugs
// obvious instead of silently dropping fields.
//
// This test is named to mirror the user-side guard
// (`TestUserMessage_OBOReservedKeysStripped` in modules/message) so a
// grep over the codebase finds both halves of the contract.
func TestBotMessage_OBOReservedKeysKept(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const (
		botID  = "bot_legacy"
		owner  = "creator_uid"
		authSp = "space_A"
	)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-bot-keeps-reject"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: authSp},
		dispatchOverride: dc.hook,
		oboStoreOverride: newFakeOBOStore(),
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   owner,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":           "trying to bypass gate 3",
			"type":              1,
			"__obo_processed__": true, // <-- malicious / forbidden
		},
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: owner})

	ba.sendMessage(c)

	// Reject must carry a body that mentions the prefix so bot authors
	// can grep for it in their logs.
	if !strings.Contains(rec.Body.String(), "__obo_") {
		t.Fatalf("expected bot-API reject body to mention __obo_ prefix, got %s", rec.Body.String())
	}
	// And no dispatch (= the strip-and-pass behavior the user ingress
	// uses MUST NOT have leaked into the bot ingress).
	if dc.captured != nil {
		t.Fatalf("bot ingress must REJECT (not strip) reserved OBO keys; dispatch fired with %+v", dc.captured)
	}
}
