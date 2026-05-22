// Package bot_api · Mininglamp-OSS/octo-server#135 (YUJ-1762) — Unit
// tests for the bot-token endpoint GET /v1/bot/obo-grant.
//
// The endpoint sits behind ba.authBot() in production, but these tests
// drive the handler directly with a fake bot-id context (same shape the
// middleware sets) so we can exercise the three branches mandated by
// the issue spec — grant present, no active grant, empty
// persona_prompt — without standing up MySQL or the auth middleware.
package bot_api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// makeBotCtx — bot-token analogue of makeCtx. The auth middleware sets
// CtxKeyRobotID after authenticating; tests skip the middleware and set
// the value directly so the handler observes the same context shape it
// would in production.
func makeBotCtx(t *testing.T, botUID string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v1/bot/obo-grant", nil)
	gc.Request = req
	c := &wkhttp.Context{Context: gc}
	if botUID != "" {
		c.Set(CtxKeyRobotID, botUID)
	}
	return c, rec
}

// decodeBotGrantResp — wkhttp's `c.Response(...)` writes the payload
// straight to the body without an envelope, so we unmarshal directly
// into the response struct.
func decodeBotGrantResp(t *testing.T, body []byte) oboBotGetGrantResp {
	t.Helper()
	var resp oboBotGetGrantResp
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v / body=%s", err, string(body))
	}
	return resp
}

// TestOBO_BotGetGrant_Happy — issue requirement #1: when an active
// grant exists for the calling bot, return 200 with grantor_uid,
// persona_prompt, active=true, and global_enabled mirroring the row.
func TestOBO_BotGetGrant_Happy(t *testing.T) {
	const (
		grantor = "admin"
		botUID  = "bot_clone_135"
		prompt  = "始终使用英语来回复"
	)
	s := newFakeOBOStore()
	s.seedBot(botUID, grantor)
	id, err := s.insertGrant(grantor, botUID, "auto", prompt)
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	// insertGrant defaults global_enabled=0; flip it to 1 so the
	// response demonstrates both booleans round-tripping faithfully.
	one := 1
	if err := s.updateGrant(id, "", &one, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: s,
	}
	c, rec := makeBotCtx(t, botUID)
	ba.oboBotGetGrant(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBotGrantResp(t, rec.Body.Bytes())
	if got.GrantorUID != grantor {
		t.Errorf("GrantorUID = %q, want %q", got.GrantorUID, grantor)
	}
	if got.PersonaPrompt != prompt {
		t.Errorf("PersonaPrompt = %q, want %q", got.PersonaPrompt, prompt)
	}
	if !got.Active {
		t.Errorf("Active = false, want true")
	}
	if !got.GlobalEnabled {
		t.Errorf("GlobalEnabled = false, want true")
	}
}

// TestOBO_BotGetGrant_NoGrant — issue requirement #2: 404 when there
// is no active grant for the calling bot. Covers both "grant never
// existed" (this test) and "grant was revoked" (the revoked variant
// below) — both paths surface the same 404 so the adapter does not
// need to distinguish the cause.
func TestOBO_BotGetGrant_NoGrant(t *testing.T) {
	s := newFakeOBOStore()
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: s,
	}
	c, rec := makeBotCtx(t, "bot_with_no_grants")
	ba.oboBotGetGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

// TestOBO_BotGetGrant_RevokedGrant — the 404 path must also cover
// grants that exist but have been revoked. `revokeGrant` flips
// active=0 / global_enabled=0 / sets revoked_at, and our
// findActiveGrantByBot SELECT filters on `active=1`, so the row must
// not surface.
func TestOBO_BotGetGrant_RevokedGrant(t *testing.T) {
	const (
		grantor = "admin"
		botUID  = "bot_revoked_135"
	)
	s := newFakeOBOStore()
	s.seedBot(botUID, grantor)
	id, err := s.insertGrant(grantor, botUID, "auto", "previous prompt")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	if err := s.revokeGrant(id); err != nil {
		t.Fatalf("revokeGrant: %v", err)
	}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: s,
	}
	c, rec := makeBotCtx(t, botUID)
	ba.oboBotGetGrant(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404 for revoked grant",
			rec.Code, rec.Body.String())
	}
}

// TestOBO_BotGetGrant_EmptyPersonaPrompt — issue requirement #3:
// when the grant was created without a persona_prompt the response
// must surface an empty string (NOT a null / missing field).
// `insertGrant` writes "" when called with an empty prompt; the
// production SQL relies on COALESCE(persona_prompt,'') for legacy NULL
// rows, so the fake's "" matches the prod wire shape.
func TestOBO_BotGetGrant_EmptyPersonaPrompt(t *testing.T) {
	const (
		grantor = "admin"
		botUID  = "bot_no_prompt_135"
	)
	s := newFakeOBOStore()
	s.seedBot(botUID, grantor)
	if _, err := s.insertGrant(grantor, botUID, "auto", ""); err != nil {
		t.Fatalf("insertGrant: %v", err)
	}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: s,
	}
	c, rec := makeBotCtx(t, botUID)
	ba.oboBotGetGrant(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	got := decodeBotGrantResp(t, rec.Body.Bytes())
	if got.PersonaPrompt != "" {
		t.Errorf("PersonaPrompt = %q, want empty string", got.PersonaPrompt)
	}
	// Defensive: the JSON itself must contain `"persona_prompt":""`,
	// not `"persona_prompt":null` — encoding/json on a `string` field
	// emits an explicit empty string, but if a future refactor swaps
	// to `*string` to model NULL we want the test to fail loudly.
	if !containsBytes(rec.Body.Bytes(), []byte(`"persona_prompt":""`)) {
		t.Errorf("response body must contain `\"persona_prompt\":\"\"`, got %s",
			rec.Body.String())
	}
	if !got.Active {
		t.Errorf("Active = false, want true (insertGrant defaults active=1)")
	}
	// insertGrant defaults global_enabled=0 — the response must report
	// that faithfully so the adapter knows the persona is paused at the
	// global switch even though the row is active.
	if got.GlobalEnabled {
		t.Errorf("GlobalEnabled = true, want false (insertGrant default)")
	}
}

// TestOBO_BotGetGrant_DeterministicMultiGrantor — when a bot is the
// grantee of multiple grantors' active grants (rare, but possible),
// the response must be deterministic across polls. Pins down the
// `ORDER BY id ASC LIMIT 1` contract documented on findActiveGrantByBot:
// the smallest-ID active row wins.
func TestOBO_BotGetGrant_DeterministicMultiGrantor(t *testing.T) {
	const botUID = "bot_multi_grantor_135"
	s := newFakeOBOStore()
	s.seedBot(botUID, "first_grantor")
	first, err := s.insertGrant("first_grantor", botUID, "auto", "first")
	if err != nil {
		t.Fatalf("insertGrant first: %v", err)
	}
	second, err := s.insertGrant("second_grantor", botUID, "auto", "second")
	if err != nil {
		t.Fatalf("insertGrant second: %v", err)
	}
	if first >= second {
		t.Fatalf("test setup: expected first.id (%d) < second.id (%d)", first, second)
	}

	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: s,
	}
	for i := 0; i < 3; i++ {
		c, rec := makeBotCtx(t, botUID)
		ba.oboBotGetGrant(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("iteration %d: status=%d", i, rec.Code)
		}
		got := decodeBotGrantResp(t, rec.Body.Bytes())
		if got.GrantorUID != "first_grantor" {
			t.Errorf("iteration %d: GrantorUID = %q, want %q (smallest-id wins)",
				i, got.GrantorUID, "first_grantor")
		}
	}
}

// TestOBO_BotGetGrant_NoBotID — defensive: if the auth middleware ever
// drops the robot id, the handler must surface an internal-error
// response rather than silently returning someone else's grant.
func TestOBO_BotGetGrant_NoBotID(t *testing.T) {
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-test"),
		oboStoreOverride: newFakeOBOStore(),
	}
	c, rec := makeBotCtx(t, "") // no CtxKeyRobotID set
	ba.oboBotGetGrant(c)
	if rec.Code == http.StatusOK {
		t.Fatalf("status=%d, want non-200 when bot id is missing", rec.Code)
	}
}

// containsBytes is a tiny inline contains check — kept private so it
// does not collide with helpers in other test files.
func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
