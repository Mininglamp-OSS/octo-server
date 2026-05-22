// Package bot_api · YUJ-1465 / Mininglamp-OSS/octo-server#108 — OBO v2
// regression coverage for the four spec items the v2 task lands:
//
//   1. fan-out trigger narrowing (mention.uids must contain grantor for
//      group / community-topic traffic; @AI / @bot / plain messages do
//      not summon the persona);
//   2. fan-out payload — v2 obo_grantor_uid / obo_grantor_name /
//      obo_respond_as / obo_system_hint, with persona_prompt appended;
//   3. fan-out response target — adapter-owned but the payload carries
//      obo_origin_channel_id / obo_origin_channel_type / obo_grantor_uid
//      so the adapter has every field it needs to route a reply back;
//   5. grant mutual exclusion — creating / reactivating a grant
//      atomically demotes every other active grant for the same
//      grantor;
//   6. persona_prompt — CRUD round-trip (create / update / list).
//
// Item 4 (typing indicator OBO identity) lives in obo_v2_typing_test.go
// alongside the existing typing test surface.
package bot_api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// ====================================================================
// 1. Fan-out narrowing (mention.uids gate)
// ====================================================================

// TestFanout_V2_GroupRequiresGrantorMention — a group message with NO
// mention.uids field must NOT fan out under v2. Locks the spec narrow:
// "Do NOT fan-out for @AI, @bot, or plain messages".
func TestFanout_V2_GroupRequiresGrantorMention(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"plain group message, no mention"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("v2 narrowing violated: plain group msg must NOT summon persona, got %d dispatches", n)
	}
	if len(fc.copies) != 0 {
		t.Fatalf("captured %d copies, expected 0", len(fc.copies))
	}
}

// TestFanout_V2_GroupMentionAIBotDoesNotSummonPersona — a message that
// @-mentions only the bot or only sets `mention.ais=1` (the AI-broadcast
// flag) must not summon the grantor's persona. The narrowing gate is
// strict on `mention.uids` for AI-only / bot-only traffic — neither
// `mention.ais` nor a foreign-bot uid in `mention.uids` counts. Without
// this strictness the v1 "fan out everything" semantics return.
//
// YUJ-1538 carve-out: `mention.all=1` (the `@所有人` broadcast flag) IS
// honoured by the gate as an implicit summon for every grantor, so it
// is intentionally NOT exercised here — the dedicated
// TestFanout_V2_GroupMentionAllSummonsPersona below covers that.
func TestFanout_V2_GroupMentionAIBotDoesNotSummonPersona(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	// mention.ais=1 + mention.uids=[some_other_bot] — neither summons
	// our grantor (tGrantor) so v2 must skip.
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@AI please","mention":{"ais":1,"uids":["some_other_bot"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 0 {
		t.Fatalf("v2 narrowing violated: @AI / mention.ais must NOT summon persona without explicit @grantor or mention.all=1, got %d dispatches", n)
	}
}

// TestFanout_V2_GroupWithGrantorMentionFansOut — happy path under v2:
// mention.uids explicitly contains the grantor → fan-out fires.
func TestFanout_V2_GroupWithGrantorMentionFansOut(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu can you help?","mention":{"uids":["` + tGrantor + `","someone_else"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("v2 happy path: explicit @grantor in mention.uids must fan out, got %d", n)
	}
	if len(fc.copies) != 1 {
		t.Fatalf("captured %d copies, expected 1", len(fc.copies))
	}
}

// TestFanout_V2_DMDoesNotRequireMention — DMs are 1:1 with the
// recipient (grantor), so the v2 narrowing gate does NOT require an
// explicit @grantor for DM payloads. Mirrors the practical reality
// that DM payloads carry no mention.uids array.
func TestFanout_V2_DMDoesNotRequireMention(t *testing.T) {
	const peer = "u_bob"
	ct := common.ChannelTypePerson.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	if _, err := s.insertScope(gid, peer, ct, 1); err != nil {
		t.Fatalf("insertScope: %v", err)
	}
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     peer,
		ChannelID:   tGrantor, // DM recipient = grantor (listener-native view)
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"hey, can we chat?"}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("DM fan-out should NOT require explicit mention.uids (recipient is the implicit summon), got %d", n)
	}
}

// ====================================================================
// 2. Fan-out payload — v2 fields + obo_system_hint composition
// ====================================================================

// TestFanout_V2_PayloadCarriesGrantorFields — the dispatched copy must
// carry obo_grantor_uid / obo_grantor_name / obo_respond_as so the
// adapter can route the bot's reply back as the grantor.
func TestFanout_V2_PayloadCarriesGrantorFields(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	ba.oboDisplayNameLookup = func(uid string) string {
		switch uid {
		case tGrantor:
			return "禹"
		case "alice":
			return "Alice"
		}
		return ""
	}
	ba.oboGroupNameLookup = func(channelID string, _ uint8) string {
		if channelID == ch {
			return "项目周会"
		}
		return ""
	}

	msg := &config.MessageResp{
		FromUID:      "alice",
		ChannelID:    ch,
		ChannelType:  ct,
		MessageIDStr: "msg_123",
		Payload:      []byte(`{"type":1,"content":"@yu 看一下","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("expected 1 fan-out, got %d", n)
	}
	cp := fc.copies[0]
	var p map[string]interface{}
	if err := json.Unmarshal(cp.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := p["obo_grantor_uid"]; got != tGrantor {
		t.Fatalf("obo_grantor_uid: want %q got %v", tGrantor, got)
	}
	if got := p["obo_grantor_name"]; got != "禹" {
		t.Fatalf("obo_grantor_name: want 禹 got %v", got)
	}
	if got := p["obo_respond_as"]; got != tGrantor {
		t.Fatalf("obo_respond_as: want %q got %v", tGrantor, got)
	}
	if got := p["obo_origin_channel_id"]; got != ch {
		t.Fatalf("obo_origin_channel_id: want %q got %v", ch, got)
	}
	// v2 canonical message id key (octo-server#108 spec) — both
	// `obo_origin_message_id` and the legacy `obo_origin_message_idstr`
	// must be present so legacy adapters keep working.
	if got := p["obo_origin_message_id"]; got != "msg_123" {
		t.Fatalf("obo_origin_message_id: want msg_123 got %v", got)
	}
	if got := p["obo_origin_message_idstr"]; got != "msg_123" {
		t.Fatalf("obo_origin_message_idstr (legacy): want msg_123 got %v", got)
	}
	hint, _ := p["obo_system_hint"].(string)
	if hint == "" {
		t.Fatalf("obo_system_hint must be populated, got empty")
	}
	if !strings.Contains(hint, "禹") {
		t.Fatalf("obo_system_hint must contain grantor name 禹, got %q", hint)
	}
	if !strings.Contains(hint, "项目周会") {
		t.Fatalf("obo_system_hint must contain group name, got %q", hint)
	}
	if !strings.Contains(hint, "Alice") {
		t.Fatalf("obo_system_hint must contain sender name, got %q", hint)
	}
}

// TestFanout_V2_PayloadAppendsPersonaPrompt — when the grant carries
// a non-empty persona_prompt, the prompt is appended to the auto hint
// after a blank-line separator.
func TestFanout_V2_PayloadAppendsPersonaPrompt(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(tGrantor, tBot, "auto", "You are a thoughtful and concise replacement. Always answer in English.")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	_, _ = s.insertScope(gid, ch, ct, 1)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)
	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu help","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	if n := ba.fanoutForMessage(msg); n != 1 {
		t.Fatalf("expected 1 fan-out, got %d", n)
	}
	var p map[string]interface{}
	_ = json.Unmarshal(fc.copies[0].Payload, &p)
	hint, _ := p["obo_system_hint"].(string)
	if !strings.Contains(hint, "You are a thoughtful and concise replacement") {
		t.Fatalf("obo_system_hint must append persona_prompt, got %q", hint)
	}
	// Separator between auto hint and persona prompt = blank line.
	if !strings.Contains(hint, "\n\nYou are a thoughtful") {
		t.Fatalf("obo_system_hint must put persona_prompt after a blank line, got %q", hint)
	}
}

// TestFanout_V2_PayloadFromUIDIsGrantor — the fan-out copy's FromUID
// stays the grantor (PR#82 R6 P0 invariant). The v2 fields layer on
// top; they do not change the addressing.
func TestFanout_V2_PayloadFromUIDIsGrantor(t *testing.T) {
	ch, ct := "group_42", common.ChannelTypeGroup.Uint8()
	s := seedGrantWithScope(t, ch, ct)
	fc := &fanoutCapture{}
	ba := newBAforFanout(s, fc)

	msg := &config.MessageResp{
		FromUID:     "alice",
		ChannelID:   ch,
		ChannelType: ct,
		Payload:     []byte(`{"type":1,"content":"@yu","mention":{"uids":["` + tGrantor + `"]}}`),
	}
	_ = ba.fanoutForMessage(msg)
	if len(fc.copies) != 1 {
		t.Fatalf("expected 1 copy, got %d", len(fc.copies))
	}
	if fc.copies[0].FromUID != tGrantor {
		t.Fatalf("FromUID must be grantor, got %q", fc.copies[0].FromUID)
	}
}

// ====================================================================
// 5. Grant mutual exclusion (one active persona per grantor)
// ====================================================================

const (
	tV2RESTOwner = "user_owner_v2"
	tV2RESTBot1  = "bot_persona_one"
	tV2RESTBot2  = "bot_persona_two"
)

// newBAforRESTV2 — minimal BotAPI for the OBO REST handlers exercised
// in the v2 mutex / persona_prompt tests. Mirrors newBAforREST but
// seeds the bot-ownership map so oboCreateGrant's owner check passes
// for the canonical v2 fixtures.
func newBAforRESTV2(s *fakeOBOStore) *BotAPI {
	s.seedBot(tV2RESTBot1, tV2RESTOwner)
	s.seedBot(tV2RESTBot2, tV2RESTOwner)
	return &BotAPI{
		Log:              log.NewTLog("BotAPI-rest-v2"),
		oboStoreOverride: s,
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return true, nil
		},
	}
}

// TestOBO_V2_CreateGrant_DemotesPriorActiveGrant — creating a second
// grant under the same grantor must demote the first one (active=0).
// Only one persona per user.
func TestOBO_V2_CreateGrant_DemotesPriorActiveGrant(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	// First grant.
	c1, rec1 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first create: status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	var first oboGrantModel
	_ = json.Unmarshal(rec1.Body.Bytes(), &first)
	if first.Active != 1 {
		t.Fatalf("first grant should be active=1, got %d", first.Active)
	}

	// Second grant under the same owner — must demote the first.
	c2, rec2 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot2}, nil)
	ba.oboCreateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second create: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var second oboGrantModel
	_ = json.Unmarshal(rec2.Body.Bytes(), &second)
	if second.Active != 1 {
		t.Fatalf("second (newly-created) grant should be active=1, got %d", second.Active)
	}
	// First grant must now be demoted.
	demoted, _ := s.findGrantByID(first.ID)
	if demoted == nil {
		t.Fatalf("first grant disappeared")
	}
	if demoted.Active != 0 {
		t.Fatalf("v2 mutex violated: first grant should be active=0 after second grant created, got %d", demoted.Active)
	}
	if demoted.GlobalEnabled != 0 {
		t.Fatalf("v2 mutex: demoted grant must also have global_enabled=0, got %d", demoted.GlobalEnabled)
	}
	// YUJ-1744 / PR#131 R4 — siblings demoted by create-mutex are PAUSED,
	// not REVOKED. revoked_at stays NULL so the user can switch BACK to
	// this grant via a later PUT {active:1} without hitting the
	// oboUpdateGrant RevokedAt-gate. The audit timestamp is reserved
	// for the explicit DELETE path (revokeGrant).
	if demoted.RevokedAt != nil {
		t.Fatalf("v2 mutex: demoted grant must keep revoked_at=NULL (paused != revoked), got %v", demoted.RevokedAt)
	}
}

// TestOBO_V2_ReactivateGrant_DemotesOthers — reactivating a previously
// soft-deleted grant takes the mutex slot, demoting any other active
// grant the owner picked up in the interim.
func TestOBO_V2_ReactivateGrant_DemotesOthers(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	// Create grant #1, then delete (soft-delete) it.
	c1, _ := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c1)
	var first oboGrantModel
	_ = json.Unmarshal(makeRecBody(t, ba, tV2RESTOwner, "GET", "/v1/obo/grants", nil, nil, ba.oboListGrants), &first)
	g1, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)
	_ = s.revokeGrant(g1.ID)

	// Create grant #2 — now the only active grant.
	c2, _ := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot2}, nil)
	ba.oboCreateGrant(c2)
	g2, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot2)
	if g2.Active != 1 {
		t.Fatalf("grant #2 should be active=1 before reactivation, got %d", g2.Active)
	}

	// Re-create grant #1 (triggers reactivation path) → mutex must
	// demote grant #2.
	c3, rec3 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("reactivation: status=%d body=%s", rec3.Code, rec3.Body.String())
	}
	g1After, _ := s.findGrantByID(g1.ID)
	if g1After == nil || g1After.Active != 1 {
		t.Fatalf("grant #1 should be reactivated, got %+v", g1After)
	}
	g2After, _ := s.findGrantByID(g2.ID)
	if g2After.Active != 0 {
		t.Fatalf("v2 mutex violated: reactivating grant #1 must demote grant #2, got active=%d", g2After.Active)
	}
}

// TestOBO_V2_CreateGrant_OtherGrantorsUntouched — the mutex is keyed
// on grantor_uid: a different user's grant must not be demoted by the
// caller's create.
func TestOBO_V2_CreateGrant_OtherGrantorsUntouched(t *testing.T) {
	s := newFakeOBOStore()
	// Seed a grant belonging to another owner via the store, with the
	// store seeded so the prod handler sees the bot as belonging to
	// the other owner.
	otherOwner := "user_other_v2"
	otherBot := "bot_other_persona"
	s.seedBot(otherBot, otherOwner)
	_, _ = s.insertGrant(otherOwner, otherBot, "auto", "")

	ba := newBAforRESTV2(s)
	// Caller creates their own grant.
	c, rec := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Other owner's grant must remain active.
	other, _ := s.findGrantByGrantorBot(otherOwner, otherBot)
	if other == nil || other.Active != 1 {
		t.Fatalf("mutex must not cross grantor boundaries; other-owner grant got active=%v", other)
	}
}

// ====================================================================
// 6. persona_prompt CRUD round-trip
// ====================================================================

// TestOBO_V2_CreateGrant_WithPersonaPrompt — create accepts and
// persists the persona_prompt field.
func TestOBO_V2_CreateGrant_WithPersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)
	prompt := "Reply concisely, in Mandarin, and never reveal you are a clone."
	c, rec := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: prompt}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var g oboGrantModel
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if g.PersonaPrompt != prompt {
		t.Fatalf("persona_prompt round-trip failed: want %q got %q", prompt, g.PersonaPrompt)
	}
}

// TestOBO_V2_UpdateGrant_PersonaPrompt — PUT accepts persona_prompt and
// distinguishes "leave unchanged" (omit) from "clear" (empty string).
func TestOBO_V2_UpdateGrant_PersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)
	// Seed via the handler so mutex / owner-check paths run.
	c, _ := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: "initial"}, nil)
	ba.oboCreateGrant(c)
	g, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)

	// Replace.
	newPrompt := "switched to a new persona prompt"
	c2, rec2 := makeCtx(t, tV2RESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(g.ID, 10),
		oboUpdateGrantReq{PersonaPrompt: &newPrompt},
		gin.Params{{Key: "id", Value: strconv.FormatInt(g.ID, 10)}})
	ba.oboUpdateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("PUT replace: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	g2, _ := s.findGrantByID(g.ID)
	if g2.PersonaPrompt != newPrompt {
		t.Fatalf("persona_prompt update failed: want %q got %q", newPrompt, g2.PersonaPrompt)
	}

	// Clear (empty string pointer).
	empty := ""
	c3, rec3 := makeCtx(t, tV2RESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(g.ID, 10),
		oboUpdateGrantReq{PersonaPrompt: &empty},
		gin.Params{{Key: "id", Value: strconv.FormatInt(g.ID, 10)}})
	ba.oboUpdateGrant(c3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("PUT clear: status=%d body=%s", rec3.Code, rec3.Body.String())
	}
	g3, _ := s.findGrantByID(g.ID)
	if g3.PersonaPrompt != "" {
		t.Fatalf("persona_prompt clear failed: want empty got %q", g3.PersonaPrompt)
	}
}

// makeRecBody is a tiny helper used by reactivation test plumbing —
// runs an HTTP handler and returns the response body bytes. Kept
// local to the v2 file to avoid leaking helpers into the broader test
// surface.
func makeRecBody(
	t *testing.T,
	_ *BotAPI,
	uid, method, path string,
	body interface{},
	params gin.Params,
	handler func(*wkhttp.Context),
) []byte {
	t.Helper()
	c, rec := makeCtx(t, uid, method, path, body, params)
	handler(c)
	return rec.Body.Bytes()
}

// ====================================================================
// YUJ-1471 / PR#109 review-blocker regression tests
// ====================================================================

// TestOBO_V2_Reactivate_ClearsStalePersonaPrompt — when a revoked grant
// is recreated with persona_prompt="" (or omitted), the previously-
// stored prompt MUST be wiped. The old behavior silently inherited the
// stale prompt, so a user who revoked a "be sarcastic" persona and then
// recreated the same (grantor, bot) pair would still get the sarcasm
// applied to fan-out. PR#109 review blocker #3.
func TestOBO_V2_Reactivate_ClearsStalePersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	// Step 1: create grant with a strong persona prompt.
	staleSpeech := "You are sarcastic. Reply with biting wit."
	c1, rec1 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: staleSpeech}, nil)
	ba.oboCreateGrant(c1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("initial create: status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	g, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)
	if g.PersonaPrompt != staleSpeech {
		t.Fatalf("prompt not persisted, got %q", g.PersonaPrompt)
	}

	// Step 2: revoke the grant.
	_ = s.revokeGrant(g.ID)

	// Step 3: re-create the (grantor, bot) pair with NO persona prompt.
	// The reactivation must wipe the stale prompt.
	c2, rec2 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reactivation: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	gAfter, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)
	if gAfter.PersonaPrompt != "" {
		t.Fatalf("reactivation must clear stale persona_prompt, got %q", gAfter.PersonaPrompt)
	}
	if gAfter.ID != g.ID {
		t.Fatalf("reactivation should reuse the same row, got id=%d want %d", gAfter.ID, g.ID)
	}
	if gAfter.Active != 1 {
		t.Fatalf("reactivated row should be active=1, got %d", gAfter.Active)
	}
}

// TestOBO_V2_Reactivate_OverwritesPersonaPromptWithNew — sibling case
// of the clear test: a non-empty new prompt also overwrites the stale
// value (always-overwrite invariant). Locks behavior in case anyone
// later "fixes" the clear path with a "non-empty only" guard.
func TestOBO_V2_Reactivate_OverwritesPersonaPromptWithNew(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	c1, _ := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: "v1 prompt"}, nil)
	ba.oboCreateGrant(c1)
	g, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)
	_ = s.revokeGrant(g.ID)

	c2, rec2 := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: "v2 prompt"}, nil)
	ba.oboCreateGrant(c2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("reactivation: status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	gAfter, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)
	if gAfter.PersonaPrompt != "v2 prompt" {
		t.Fatalf("reactivation must overwrite stale prompt with new value, got %q", gAfter.PersonaPrompt)
	}
}

// TestOBO_V2_CreateGrant_RejectsOversizedPersonaPrompt — server-side
// length cap. The persona_prompt is appended to every fan-out copy's
// system hint; an unbounded value would balloon storage + LLM token
// budgets. Reject with 400.
func TestOBO_V2_CreateGrant_RejectsOversizedPersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	huge := strings.Repeat("x", oboPersonaPromptMaxBytes+1)
	c, rec := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: huge}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized persona_prompt, got %d body=%s", rec.Code, rec.Body.String())
	}
	// Confirm no row was written.
	if g, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1); g != nil {
		t.Fatalf("oversized create must not insert a row, got %+v", g)
	}
}

// TestOBO_V2_CreateGrant_AcceptsMaxSizedPersonaPrompt — boundary check:
// exactly the cap is allowed.
func TestOBO_V2_CreateGrant_AcceptsMaxSizedPersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	atCap := strings.Repeat("y", oboPersonaPromptMaxBytes)
	c, rec := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1, PersonaPrompt: atCap}, nil)
	ba.oboCreateGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for at-cap persona_prompt, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestOBO_V2_UpdateGrant_RejectsOversizedPersonaPrompt — mirror of the
// create cap for the PUT path.
func TestOBO_V2_UpdateGrant_RejectsOversizedPersonaPrompt(t *testing.T) {
	s := newFakeOBOStore()
	ba := newBAforRESTV2(s)

	c1, _ := makeCtx(t, tV2RESTOwner, http.MethodPost, "/v1/obo/grants",
		oboCreateGrantReq{GranteeBotUID: tV2RESTBot1}, nil)
	ba.oboCreateGrant(c1)
	g, _ := s.findGrantByGrantorBot(tV2RESTOwner, tV2RESTBot1)

	huge := strings.Repeat("z", oboPersonaPromptMaxBytes+1)
	c2, rec2 := makeCtx(t, tV2RESTOwner, http.MethodPut,
		"/v1/obo/grants/"+strconv.FormatInt(g.ID, 10),
		oboUpdateGrantReq{PersonaPrompt: &huge},
		gin.Params{{Key: "id", Value: strconv.FormatInt(g.ID, 10)}})
	ba.oboUpdateGrant(c2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized persona_prompt on PUT, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}
