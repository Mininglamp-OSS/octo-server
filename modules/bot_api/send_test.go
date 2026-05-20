// Package bot_api · YUJ-660 High-4: integration test for /v1/bot/sendMessage.
//
// Exercises the full sendMessage HTTP handler with a stubbed botSpaceQuerier
// (returns "space_A" for the test bot) and a captured dispatchOverride. The
// client POSTs payload.space_id="space_B_attacker"; the test asserts that the
// dispatched MsgSendReq carries the server-authoritative payload.space_id =
// "space_A", NOT the forged value.
//
// Path under test: BotKindUser DM (User Bot whose CreatorUID == channel_id).
// This avoids the user.IService dependency by routing through the "creator
// always allowed" branch of checkSendPermission.
package bot_api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// dispatchCapture records the MsgSendReq passed to the dispatch hook, so the
// test can decode and assert payload contents without a real WuKongIM HTTP
// dependency.
type dispatchCapture struct {
	mu       sync.Mutex
	captured *config.MsgSendReq
}

func (d *dispatchCapture) hook(req *config.MsgSendReq) (*config.MsgSendResp, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Deep-ish copy: clone the payload bytes so the assertion can't be moved
	// by later mutations of the original slice.
	clone := *req
	if req.Payload != nil {
		buf := make([]byte, len(req.Payload))
		copy(buf, req.Payload)
		clone.Payload = buf
	}
	d.captured = &clone
	return &config.MsgSendResp{MessageID: 1, MessageSeq: 1, ClientMsgNo: "test"}, nil
}

// TestSendMessage_PersonalDM_StripsForgedClientSpaceID is the YUJ-660 High-4
// acceptance test. It is the canonical regression guard for the cross-Space
// DM push leak fix on the unified Bot API path.
func TestSendMessage_PersonalDM_StripsForgedClientSpaceID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID    = "bot_X"
		creatorUID    = "user_creator"     // == channel_id, exercises creator-bypass branch
		authoritative = "space_A"          // returned by stubbed querySpaceIDByRobotID
		forged        = "space_B_attacker" // attacker-supplied value in payload
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{defaultSpace: authoritative}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	// Build the request body the client would send: PERSONAL DM with a forged
	// payload.space_id pointing to a different Space.
	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":  "hi",
			"type":     1,
			"space_id": forged,
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	// Auth context: as if authBot middleware ran for a User Bot whose creator
	// is the DM peer (creator path bypasses the IsFriend DB call).
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)

	assert.Equalf(t, http.StatusOK, rec.Code,
		"sendMessage should respond 200 OK, got %d body=%s", rec.Code, rec.Body.String())

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured, "dispatch hook must have been invoked") {
		return
	}
	assert.Equal(t, common.ChannelTypePerson.Uint8(), dc.captured.ChannelType)
	assert.Equal(t, creatorUID, dc.captured.ChannelID)
	assert.Equal(t, botRobotID, dc.captured.FromUID)

	var dispatchedPayload map[string]interface{}
	if !assert.NoError(t, json.Unmarshal(dc.captured.Payload, &dispatchedPayload)) {
		return
	}
	assert.Equal(t, authoritative, dispatchedPayload["space_id"],
		"dispatched payload.space_id MUST be the server-authoritative value, NOT the forged client value")
	assert.NotEqual(t, forged, dispatchedPayload["space_id"],
		"forged client space_id must be overridden")

	// Sanity: the stubbed querier was invoked with the correct robotID.
	assert.Equal(t, []string{botRobotID}, q.calls)
}

// TestSendMessage_PersonalDM_OrphanBotEmptyAuthoritative covers the
// observability-warn branch: the bot has no Space (ErrNotFound or "" from
// querySpaceIDByRobotID), and the client did NOT supply space_id. The
// dispatched payload should NOT contain a forged space_id and the helper
// should not crash; this is a smoke test for the empty-authoritative path.
func TestSendMessage_PersonalDM_OrphanBotNoForgedClientSpaceID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID = "orphan_bot"
		creatorUID = "user_creator"
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{} // returns ("", nil) — no space, no error

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content": "hi",
			"type":    1,
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)
	assert.Equal(t, http.StatusOK, rec.Code)

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured) {
		return
	}
	var dispatchedPayload map[string]interface{}
	assert.NoError(t, json.Unmarshal(dc.captured.Payload, &dispatchedPayload))
	_, hasSpaceID := dispatchedPayload["space_id"]
	assert.False(t, hasSpaceID,
		"orphan bot with no client-supplied space_id → dispatched payload must not contain space_id")
}

// TestSendMessage_PersonalDM_OrphanBot_ForgedClientSpaceID_Stripped is the
// YUJ-660 R3 Finding A regression guard. An orphan bot (querySpaceIDByRobotID
// returns "" with no error) combined with an attacker-forged
// payload.space_id MUST result in payload.space_id being stripped from the
// dispatched MsgSendReq. Before the fix this test would FAIL — the dispatched
// payload preserved the forged client value, leaking across Spaces.
func TestSendMessage_PersonalDM_OrphanBot_ForgedClientSpaceID_Stripped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID = "orphan_bot"
		creatorUID = "user_creator"
		forged     = "space_B_attacker"
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{} // returns ("", nil) — orphan bot, no error

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":  "hi",
			"type":     1,
			"space_id": forged,
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)
	assert.Equal(t, http.StatusOK, rec.Code)

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured) {
		return
	}
	var dispatchedPayload map[string]interface{}
	assert.NoError(t, json.Unmarshal(dc.captured.Payload, &dispatchedPayload))
	_, hasSpaceID := dispatchedPayload["space_id"]
	assert.False(t, hasSpaceID,
		"orphan bot + forged client space_id MUST be stripped from dispatched payload (fail-closed)")
}

// TestSendMessage_PersonalDM_DBError_ForgedClientSpaceID_Stripped is the
// YUJ-660 R3 Finding A regression guard for the real-DB-error branch. When
// querySpaceIDByRobotID returns a real error (network blip / failover), the
// resolver returns "" — and the helper MUST still strip the client's forged
// payload.space_id rather than passing it through. Without this protection,
// an attacker can synthesize transient DB conditions to bypass authoritative
// override.
func TestSendMessage_PersonalDM_DBError_ForgedClientSpaceID_Stripped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID = "bot_with_db_error"
		creatorUID = "user_creator"
		forged     = "space_B_attacker"
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{defaultErr: errors.New("connection refused")}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"content":  "hi",
			"type":     1,
			"space_id": forged,
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)
	assert.Equal(t, http.StatusOK, rec.Code)

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured) {
		return
	}
	var dispatchedPayload map[string]interface{}
	assert.NoError(t, json.Unmarshal(dc.captured.Payload, &dispatchedPayload))
	_, hasSpaceID := dispatchedPayload["space_id"]
	assert.False(t, hasSpaceID,
		"DB error + forged client space_id MUST be stripped from dispatched payload — attackers cannot use transient DB failure to bypass authoritative override")
}

// TestSendMessage_MentionAllRewritten_HandlerIntegration is the YUJ-1343 /
// Mininglamp-OSS/octo-server#94 acceptance test for the mention three-state
// rewrite on the /v1/bot/sendMessage handler path, updated for Plan X
// (YUJ-1389).
//
// This is the "handler-level integration test" the issue calls out: drive
// BotAPI.sendMessage end-to-end with a `payload.mention.all=1` body and
// assert the captured MsgSendReq carries `mention.ais=1` (Plan X
// chokepoint rewrite ran — legacy `@所有人` auto-fans-out to all AI bots
// without an SDK update) AND still carries `mention.all=1` (outbound
// double-write preserved for legacy read-side clients). humans MUST
// stay absent — humans is the explicit human-notification signal and is
// never inferred from a legacy `all=1`. Lesson from PR#82 OBO fan-out:
// when an external-service shape constraint matters, the test MUST go
// through the real handler stack — not call the helper in isolation —
// otherwise a wiring regression (e.g. someone deletes the call site by
// mistake) slips past unit coverage.
//
// Uses the existing creator-DM path (BotKindUser whose CreatorUID ==
// channel_id) so the test does not need a live IsFriend / group_member
// table. The rewrite call site itself is placed OUTSIDE the
// `ChannelTypePerson` conditional (F2 — see modules/bot_api/send.go), so
// even though this test drives a DM, the helper still runs; the same
// helper would run on the group / community-topic path by inspection.
func TestSendMessage_MentionAllRewritten_HandlerIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID = "bot_mention_rewrite_it"
		creatorUID = "user_creator_mention_it"
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-mention-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"type":    1,
			"content": "@所有人 ping",
			// Legacy @所有人 inbound — chokepoint MUST rewrite this.
			"mention": map[string]interface{}{
				"all": 1,
			},
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)

	assert.Equalf(t, http.StatusOK, rec.Code,
		"sendMessage should respond 200 OK, got %d body=%s", rec.Code, rec.Body.String())

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured, "dispatch hook must have been invoked") {
		return
	}

	var dispatchedPayload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(dc.captured.Payload))
	dec.UseNumber()
	if !assert.NoError(t, dec.Decode(&dispatchedPayload)) {
		return
	}
	mention, ok := dispatchedPayload["mention"].(map[string]interface{})
	if !assert.True(t, ok, "dispatched payload must keep mention map; got %T", dispatchedPayload["mention"]) {
		return
	}
	// Plan X chokepoint rewrite ran — ais=1 is now present (legacy
	// `@所有人` auto-fans-out to all AI bots without an SDK update).
	ais, _ := mention["ais"].(json.Number)
	assert.Equal(t, "1", ais.String(),
		"Plan X: BotAPI.sendMessage must invoke mentionrewrite.RewriteMention so mention.ais=1 reaches the dispatcher")
	// Outbound double-write — legacy all=1 still present so old read-side
	// clients keep rendering the @所有人 pill until they roll out support
	// for the humans/ais fields.
	all, _ := mention["all"].(json.Number)
	assert.Equal(t, "1", all.String(),
		"legacy mention.all=1 MUST be preserved on the dispatched payload (outbound double-write)")
	// humans MUST stay absent — Plan X: humans is the explicit human-
	// notification signal and is NEVER inferred from a legacy `all=1`.
	// Only the sender may set humans, and only when they want a
	// channel-level "[有人@我]" reminder for human members.
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans,
		"BotAPI.sendMessage rewrite must NOT auto-set mention.humans — humans is an explicit opt-in signal")
}

// TestSendMessage_MentionAisPassthrough_HandlerIntegration verifies the
// other end of the matrix: an explicit `mention.ais=1` from a new client
// passes through the chokepoint untouched (the Plan X rewrite is a
// one-way "all → also ais" rule, never adds humans/ais from thin air
// when the inbound shape did not request the broadcast via all=1).
func TestSendMessage_MentionAisPassthrough_HandlerIntegration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botRobotID = "bot_ais_it"
		creatorUID = "user_creator_ais_it"
	)

	dc := &dispatchCapture{}
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-ais-it"),
		spaceQuerier:     q,
		dispatchOverride: dc.hook,
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   creatorUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: map[string]interface{}{
			"type":    1,
			"content": "@所有 AI ping",
			"mention": map[string]interface{}{
				"ais": 1,
			},
		},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botRobotID)
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: botRobotID, CreatorUID: creatorUID})

	ba.sendMessage(c)
	assert.Equal(t, http.StatusOK, rec.Code)

	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !assert.NotNil(t, dc.captured) {
		return
	}
	var dispatchedPayload map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(dc.captured.Payload))
	dec.UseNumber()
	assert.NoError(t, dec.Decode(&dispatchedPayload))
	mention := dispatchedPayload["mention"].(map[string]interface{})
	ais, _ := mention["ais"].(json.Number)
	assert.Equal(t, "1", ais.String(), "ais=1 must passthrough")
	_, hasHumans := mention["humans"]
	assert.False(t, hasHumans, "ais-only input must NOT gain humans=1")
	_, hasAll := mention["all"]
	assert.False(t, hasAll, "ais-only input must NOT gain legacy all=1")
}
