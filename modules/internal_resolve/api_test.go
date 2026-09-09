package internal_resolve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

// testInternalToken is 32 bytes so it clears the minInternalTokenBytes floor
// enforced by resolveDriveInternalToken. Keeping a single constant anchors
// the length invariant across every auth test.
const testInternalToken = "0123456789abcdef0123456789abcdef" // 32 bytes

// stubResolver is an in-memory identityResolver for tests. `resolveErr` lets
// a test drive the dependency error path without touching MySQL. The
// `identities` map maps uid → the identity Resolve should return; absent uid
// means "not a bot" (Resolve → nil, nil, matching the botidentity contract).
//
// Storing full *Identity values (not just kind + creator) lets a test express
// KindUserBot with creator OR KindAppBot with scope/space independently, which
// is precisely the P1 the review flagged: earlier tests could only represent
// robot-table truth and could not fail on an app-bot misclassification.
type stubResolver struct {
	identities map[string]*botidentity.Identity
	resolveErr error

	calls   int
	lastUID string
}

func (s *stubResolver) Resolve(uid string) (*botidentity.Identity, error) {
	s.calls++
	s.lastUID = uid
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	// nil is a valid answer (uid is not a live bot); missing key returns nil.
	return s.identities[uid], nil
}

func newTestModule(res identityResolver) *Module {
	return &Module{
		identities:    res,
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalResolveTest"),
	}
}

func newRouter(m *Module) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	// This hermetic router mounts ONLY auth + handler. The production
	// Route() shape (per-endpoint IP rate limiter → auth → handler on the
	// concrete POST) is covered by the behavioural test in
	// route_runtime_order_test.go, which uses recording middleware to
	// assert the actual runtime execution order — no live Redis needed.
	// Keeping the smoke path here Redis-free is intentional; anything that
	// truly requires Redis lives in the runtime-order test file.
	r.POST("/v1/internal/user/resolve-bot-owner", m.internalAuthMiddleware(), m.resolveBotOwner)
	return r
}

func doResolveRequest(t *testing.T, r *wkhttp.WKHttp, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case []byte:
		raw = v
	case nil:
		raw = nil
	default:
		var err error
		raw, err = json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	var bodyReader = bytes.NewReader(raw)
	req := httptest.NewRequest(http.MethodPost, "/v1/internal/user/resolve-bot-owner", bodyReader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type errorEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) resolveBotOwnerResponse {
	t.Helper()
	var resp resolveBotOwnerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %s: %v", w.Body.String(), err)
	}
	return resp
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error %s: %v", w.Body.String(), err)
	}
	return env
}

// ============================================================================
// Auth (X-Internal-Token) — mirrors modules/bot_mention/api_test.go semantics
// ============================================================================

func TestUnauthorizedWhenTokenMissing(t *testing.T) {
	r := newRouter(newTestModule(&stubResolver{}))
	w := doResolveRequest(t, r, "", resolveBotOwnerRequest{UID: "u1"})
	if got, want := w.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d, body=%s", got, want, w.Body.String())
	}
	env := decodeError(t, w)
	if env.Error.Code != "err.shared.auth.token_invalid" {
		t.Fatalf("error code = %q, want err.shared.auth.token_invalid", env.Error.Code)
	}
}

func TestUnauthorizedWhenTokenMismatch(t *testing.T) {
	r := newRouter(newTestModule(&stubResolver{}))
	w := doResolveRequest(t, r, "wrong-token", resolveBotOwnerRequest{UID: "u1"})
	if got, want := w.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestUnauthorizedWhenServerTokenUnset(t *testing.T) {
	// The middleware fails closed if the server-side token is empty even when
	// the caller supplies one. This guards misconfigured deployments.
	m := &Module{
		identities:    &stubResolver{},
		internalToken: "", // server-side unset
		Log:           log.NewTLog("InternalResolveTest"),
	}
	r := newRouter(m)
	w := doResolveRequest(t, r, "any-token", resolveBotOwnerRequest{UID: "u1"})
	if got, want := w.Code, http.StatusUnauthorized; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// ============================================================================
// Request validation
// ============================================================================

func TestInvalidBodyJSON(t *testing.T) {
	r := newRouter(newTestModule(&stubResolver{}))
	w := doResolveRequest(t, r, testInternalToken, []byte("{not json"))
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d, body=%s", got, want, w.Body.String())
	}
	env := decodeError(t, w)
	if env.Error.Code != "err.shared.param.invalid" {
		t.Fatalf("error code = %q, want err.shared.param.invalid", env.Error.Code)
	}
}

func TestInvalidBodyExtraFields(t *testing.T) {
	// DisallowUnknownFields defends against a typo silently succeeding.
	r := newRouter(newTestModule(&stubResolver{}))
	w := doResolveRequest(t, r, testInternalToken, []byte(`{"uid":"u1","extra":"nope"}`))
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestInvalidUIDEmpty(t *testing.T) {
	r := newRouter(newTestModule(&stubResolver{}))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "   "})
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestInvalidUIDTooLong(t *testing.T) {
	r := newRouter(newTestModule(&stubResolver{}))
	tooLong := strings.Repeat("x", 257)
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: tooLong})
	if got, want := w.Code, http.StatusBadRequest; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

// ============================================================================
// Happy paths — the four bot classifications the design must distinguish
// ============================================================================

// A uid that is not present in either bot table maps to robot=0.
func TestHumanUIDReturnsRobotZero(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "alice"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d, body=%s", got, want, w.Body.String())
	}
	resp := decodeResponse(t, w)
	if resp.UID != "alice" || resp.Robot != 0 || resp.BotCreatorUID != "" {
		t.Fatalf("resp = %+v, want {alice 0 \"\"}", resp)
	}
	// The endpoint must NOT hit any downstream table beyond the single
	// authoritative Resolve call — one lookup per request.
	if res.calls != 1 {
		t.Errorf("resolver.calls = %d, want 1", res.calls)
	}
}

// Same wire shape as human — unknown uid must not be distinguishable from a
// known human, or the endpoint leaks existence.
func TestUnknownUIDReturnsRobotZero(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "ghost"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.Robot != 0 || resp.BotCreatorUID != "" {
		t.Fatalf("resp = %+v, want {ghost 0 \"\"}", resp)
	}
}

// user_bot with a human creator uid — the mainstream bot case.
func TestUserBotWithCreatorReturnsRobotOne(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"bot-1": {UID: "bot-1", Kind: botidentity.KindUserBot, CreatorUID: "alice"},
	}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "bot-1"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.UID != "bot-1" || resp.Robot != 1 || resp.BotCreatorUID != "alice" {
		t.Fatalf("resp = %+v, want {bot-1 1 alice}", resp)
	}
}

// user_bot without a creator (legacy or platform-registered) — robot=1, no creator.
func TestUserBotWithoutCreatorReturnsRobotOne(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"bot-legacy": {UID: "bot-legacy", Kind: botidentity.KindUserBot, CreatorUID: ""},
	}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "bot-legacy"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.Robot != 1 || resp.BotCreatorUID != "" {
		t.Fatalf("resp = %+v, want {bot-legacy 1 \"\"}", resp)
	}
}

// Published platform-scope app_bot — MUST return robot=1 (not 0). This is
// the exact regression the review flagged: a robot-only query returned
// {robot:0, ""} for app_bot uids and the drive caller then treated the bot
// as a plain owner. Fail loud if we ever revert.
func TestPlatformAppBotReturnsRobotOneWithEmptyCreator(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"platform-appbot": {
			UID: "platform-appbot", Kind: botidentity.KindAppBot,
			AppScope: botidentity.ScopePlatform,
		},
	}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "platform-appbot"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.Robot != 1 {
		t.Fatalf("resp.Robot = %d, want 1 for KindAppBot (regression: robot-only classification returned)", resp.Robot)
	}
	if resp.BotCreatorUID != "" {
		t.Fatalf("resp.BotCreatorUID = %q, want empty for KindAppBot (app-bot has no human creator)", resp.BotCreatorUID)
	}
}

// Published space-scope app_bot — same wire shape as platform-scope app_bot;
// scope value is not currently on the wire (deliberately, see #710) but the
// classification must land on the app-bot branch.
func TestSpaceAppBotReturnsRobotOneWithEmptyCreator(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"space-appbot": {
			UID: "space-appbot", Kind: botidentity.KindAppBot,
			AppScope: botidentity.ScopeSpace, AppSpaceID: "space-1",
		},
	}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "space-appbot"})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.Robot != 1 || resp.BotCreatorUID != "" {
		t.Fatalf("resp = %+v, want {space-appbot 1 \"\"}", resp)
	}
}

func TestUIDIsTrimmedBeforeLookup(t *testing.T) {
	res := &stubResolver{identities: map[string]*botidentity.Identity{
		"bot-1": {UID: "bot-1", Kind: botidentity.KindUserBot, CreatorUID: "alice"},
	}}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "  bot-1  "})
	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	resp := decodeResponse(t, w)
	if resp.UID != "bot-1" || resp.Robot != 1 {
		t.Fatalf("resp = %+v, want trimmed uid + robot=1", resp)
	}
	if res.lastUID != "bot-1" {
		t.Errorf("resolver.lastUID = %q, want trimmed 'bot-1'", res.lastUID)
	}
}

// ============================================================================
// Downstream errors → 500
// ============================================================================

func TestResolveErrorReturns500(t *testing.T) {
	res := &stubResolver{resolveErr: errors.New("db unavailable")}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "u1"})
	if got, want := w.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d, body=%s", got, want, w.Body.String())
	}
}

// ErrAmbiguousIdentity signals a broken cross-table uniqueness invariant.
// The endpoint MUST fail closed rather than silently pick one bot kind.
func TestAmbiguousIdentityFailsClosed(t *testing.T) {
	// The exact wrapped error shape botidentity.Resolver returns for a uid
	// live in BOTH robot AND app_bot — see modules/botidentity/resolver.go.
	res := &stubResolver{resolveErr: fmt.Errorf("%w: uid %q is active in robot and app_bot",
		botidentity.ErrAmbiguousIdentity, "conflict-uid")}
	r := newRouter(newTestModule(res))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "conflict-uid"})
	if got, want := w.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d — ambiguous cross-table identity must fail closed", got, want)
	}
}

// unknownKindResolver returns an Identity with a value botidentity.Kind we do
// not recognize. This guards the exhaustive switch in resolveBotOwner: adding
// a new Kind to botidentity without teaching this module about it must trip
// this test rather than silently emit `bot_creator_uid=""` for the new class.
type unknownKindResolver struct{}

func (unknownKindResolver) Resolve(uid string) (*botidentity.Identity, error) {
	return &botidentity.Identity{UID: uid, Kind: botidentity.Kind("future_kind")}, nil
}

func TestUnknownKindFailsClosed(t *testing.T) {
	r := newRouter(newTestModule(unknownKindResolver{}))
	w := doResolveRequest(t, r, testInternalToken, resolveBotOwnerRequest{UID: "future-bot"})
	if got, want := w.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d — unknown Kind must not be silently classified", got, want)
	}
}

// ============================================================================
// Token separation config
// ============================================================================

func TestResolveDriveInternalTokenRejectsUnset(t *testing.T) {
	getenv := func(string) string { return "" }
	_, err := resolveDriveInternalToken(getenv)
	if err == nil {
		t.Fatalf("expected error when OCTO_DRIVE_INTERNAL_TOKEN is unset")
	}
}

func TestResolveDriveInternalTokenRejectsShortValue(t *testing.T) {
	// 31 bytes: below the 32-byte floor. Length check runs BEFORE collision
	// checks, so this passes without any siblings configured.
	shortTok := strings.Repeat("x", minInternalTokenBytes-1)
	getenv := func(k string) string {
		if k == DriveInternalTokenEnv {
			return shortTok
		}
		return ""
	}
	if _, err := resolveDriveInternalToken(getenv); err == nil {
		t.Fatalf("expected error for token shorter than %d bytes", minInternalTokenBytes)
	}
}

func TestResolveDriveInternalTokenRejectsSiblingCollision(t *testing.T) {
	// Use a 32-byte shared value so the length gate passes and we exercise
	// the collision cases individually.
	sharedSecret := strings.Repeat("s", minInternalTokenBytes)
	cases := []struct {
		name    string
		sibling string
	}{
		{"notify", notifyInternalTokenEnv},
		{"docs-notify", docsNotifyInternalToken},
		{"bot-mention", botMentionInternalToken},
		{"marketplace", marketplaceInternalToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == DriveInternalTokenEnv || k == tc.sibling {
					return sharedSecret
				}
				return ""
			}
			_, err := resolveDriveInternalToken(getenv)
			if err == nil {
				t.Fatalf("expected error when %s == %s", DriveInternalTokenEnv, tc.sibling)
			}
		})
	}
}

// TestMarketplaceInternalTokenLiteralMatchesSpaceConstant pins the duplicated
// env-name literal in config.go to the exported constant that owns it. The
// literal exists so this module has no production dependency on modules/space;
// the cost is that a rename over there would silently turn our collision check
// into a comparison against an env nobody sets. This test converts that silent
// failure into a build-time-visible one.
func TestMarketplaceInternalTokenLiteralMatchesSpaceConstant(t *testing.T) {
	if marketplaceInternalToken != space.MarketplaceInternalTokenEnv {
		t.Fatalf("marketplaceInternalToken = %q, want %q (modules/space renamed the env; "+
			"update config.go or the intra-set collision check silently stops working)",
			marketplaceInternalToken, space.MarketplaceInternalTokenEnv)
	}
}

func TestResolveDriveInternalTokenAcceptsUnique(t *testing.T) {
	// Both values must clear the 32-byte length floor for the check to
	// exercise ONLY the collision path.
	driveTok := strings.Repeat("d", minInternalTokenBytes)
	otherTok := strings.Repeat("o", minInternalTokenBytes)
	getenv := func(k string) string {
		if k == DriveInternalTokenEnv {
			return driveTok
		}
		return otherTok
	}
	got, err := resolveDriveInternalToken(getenv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != driveTok {
		t.Fatalf("token = %q, want %q", got, driveTok)
	}
}
