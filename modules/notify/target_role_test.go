package notify

// Tests for role-targeted delivery (NotifyReq.target_role) and for the
// cross-module fixed internal-token exclusion set.
//
// The harness (newWuKongServer / newTestContext / newMockedDBSession /
// newTestNotify / primeMemberCache / buildRouter / doJSONRequest) lives in
// notify_integration_test.go; this file reuses it rather than growing a second
// one.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// validateTargeting — "exactly one of targets / target_role"
// =============================================================================

func TestValidateTargeting(t *testing.T) {
	cases := []struct {
		name    string
		req     NotifyReq
		wantErr bool
		why     string
	}{
		{
			name:    "targets only (the existing contract)",
			req:     NotifyReq{Targets: []string{"u1"}},
			wantErr: false,
			why:     "every existing producer sends this shape and must stay unaffected",
		},
		{
			name:    "target_role only",
			req:     NotifyReq{TargetRole: TargetRoleSpaceAdmin},
			wantErr: false,
		},
		{
			name:    "both set",
			req:     NotifyReq{Targets: []string{"u1"}, TargetRole: TargetRoleSpaceAdmin},
			wantErr: true,
			why: "a producer that sends both has a bug; honouring either one by " +
				"precedence is how the wrong people get an approval card",
		},
		{
			name:    "neither set",
			req:     NotifyReq{},
			wantErr: true,
			why:     "this is what binding:\"required\" used to reject; the rule must survive",
		},
		{
			name:    "explicit empty targets, no role",
			req:     NotifyReq{Targets: []string{}},
			wantErr: true,
			why:     "an empty slice is not a recipient set; same as absent",
		},
		{
			name:    "unknown role value",
			req:     NotifyReq{TargetRole: "space_owner"},
			wantErr: true,
			why: "an unrecognized selector must be a 400, never a silent fallback — " +
				"a typo may not be allowed to widen or narrow the audience",
		},
		{
			name:    "role value with surrounding whitespace is accepted",
			req:     NotifyReq{TargetRole: "  " + TargetRoleSpaceAdmin + " "},
			wantErr: false,
		},
		{
			name:    "whitespace-only role with no targets is still 'neither'",
			req:     NotifyReq{TargetRole: "   "},
			wantErr: true,
		},
		{
			name:    "case variation is rejected",
			req:     NotifyReq{TargetRole: "SPACE_ADMIN"},
			wantErr: true,
			why:     "the vocabulary is exact; near-misses are caller bugs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTargeting(&tc.req)
			if tc.wantErr {
				require.Error(t, err, tc.why)
			} else {
				require.NoError(t, err, tc.why)
			}
		})
	}

	t.Run("nil request", func(t *testing.T) {
		require.Error(t, validateTargeting(nil))
	})
}

// =============================================================================
// HTTP surface
// =============================================================================

func newRoleTargetRouter(n *Notify) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	n.Route(r)
	return r
}

func decodeNotifyResp(t *testing.T, w *httptest.ResponseRecorder) NotifyResp {
	t.Helper()
	var resp NotifyResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func decodeNotifyErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
	return env.Error.Code
}

// Credentials and route identity for the rig below. roleActionToken is the
// per-route notify token an operator would put in OCTO_CARD_ACTION_ROUTES for
// octo-marketplace; roleLegacyToken / roleDocsToken are the two fixed
// credentials that must NOT be able to use target_role.
const (
	roleLegacyToken = "legacy-token"
	roleDocsToken   = "docs-token"
	roleActionToken = "abcdef0123456789abcdef0123456789"
	roleActionType  = "marketplace.plugin_review.decision"
	roleActionOwner = "marketplace"
)

// roleTargetingRig wires a Notify the way production wires the marketplace
// consumer: all three credential classes live side by side, so a test can post
// the SAME body under each one and compare outcomes. That is the only way to
// assert the capability gate rather than merely exercising the happy path.
type roleTargetingRig struct {
	n       *Notify
	router  *wkhttp.WKHttp
	capture *capturingCardSender
	mock    sqlmock.Sqlmock
}

func newRoleTargetingRig(t *testing.T) *roleTargetingRig {
	t.Helper()
	t.Setenv("OCTO_CARD_MESSAGE_ENABLED", "true")
	wk := newWuKongServer()
	t.Cleanup(wk.close)
	ctx := newTestContext(t, wk)
	db, mock, closeDB := newMockedDBSession(t)
	t.Cleanup(closeDB)

	registry, err := cardactiondispatch.NewRegistry([]cardactiondispatch.RouteSpec{{
		SenderUID: "notification", Owner: roleActionOwner, ActionType: roleActionType,
		URL:            "https://marketplace.internal/v1/card-actions/decide",
		SecretEnv:      "OCTO_MARKETPLACE_CARD_ACTION_SECRET",
		NotifyTokenEnv: "OCTO_MARKETPLACE_NOTIFY_TOKEN",
	}}, func(key string) string {
		switch key {
		case "OCTO_MARKETPLACE_CARD_ACTION_SECRET":
			return "0123456789abcdef0123456789abcdef"
		case "OCTO_MARKETPLACE_NOTIFY_TOKEN":
			return roleActionToken
		}
		return ""
	})
	require.NoError(t, err)
	service, err := cardactiondispatch.NewService(registry, unusedActionQueue{}, ctx)
	require.NoError(t, err)

	capability := cardactiondispatch.NotifyCapability{SenderUID: "notification", Owner: roleActionOwner}
	capture := &capturingCardSender{}
	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, roleLegacyToken)
	n.docsToken = roleDocsToken
	n.actionService = service
	n.actionSenders = map[cardactiondispatch.NotifyCapability]carddispatch.Sender{capability: capture}
	n.botOK.Store(true)

	return &roleTargetingRig{n: n, router: newRoleTargetRouter(n), capture: capture, mock: mock}
}

func (r *roleTargetingRig) post(t *testing.T, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONRequest(t, r.router, http.MethodPost, "/v1/internal/notify",
		http.Header{InternalTokenHeader: []string{token}}, body)
}

func (r *roleTargetingRig) sentCards() int {
	r.capture.mu.Lock()
	defer r.capture.mu.Unlock()
	return len(r.capture.cards)
}

// approvalCardRoleRequest is the shape octo-marketplace actually sends: no
// targets, a role selector, and the plugin-review ApprovalCard.
func approvalCardRoleRequest(spaceID string) map[string]interface{} {
	return map[string]interface{}{
		"space_id":    spaceID,
		"service":     "marketplace",
		"target_role": TargetRoleSpaceAdmin,
		"approval_card": map[string]interface{}{
			"action_type": roleActionType,
			"title":       "Plugin review pending",
			"description": "octo-cli-helper v1.2.0 awaits review",
		},
	}
}

// The happy path: the caller names no uids, octo-server resolves the Space's
// admins, and the response tells the caller exactly who was delivered to.
func TestRoleTargeting_ResolvesAdminsAndReportsDelivered(t *testing.T) {
	rig := newRoleTargetingRig(t)
	const spaceID = "sp_role_ok"
	// space.ActiveAdminUIDs
	rig.mock.ExpectQuery(`SELECT sm.uid`).
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("owner_uid").AddRow("admin_uid"))
	// memberCache.refresh (the resolved uids still go through membership
	// verification — role resolution does not bypass any existing check)
	rig.mock.ExpectQuery(`SELECT uid FROM space_member`).
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("owner_uid").AddRow("admin_uid"))

	w := rig.post(t, roleActionToken, approvalCardRoleRequest(spaceID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	resp := decodeNotifyResp(t, w)
	assert.ElementsMatch(t, []string{"owner_uid", "admin_uid"}, resp.Delivered,
		"the caller no longer knows the target set, so `delivered` is its only "+
			"record of who received the notification")
	assert.Empty(t, resp.Filtered)
	assert.Equal(t, 2, rig.sentCards())
	assert.NoError(t, rig.mock.ExpectationsWereMet())
}

// target_role is scoped to the action capability. The legacy and docs
// credentials must not be able to use it: `delivered` on a role-targeted
// request IS the Space's admin roster, so admitting either one would rebuild —
// on a different endpoint, with a shared token and no membership relationship —
// the cross-tenant roster capability this change deleted from modules/space.
func TestRoleTargeting_OnlyActionCapabilityMayUseTargetRole(t *testing.T) {
	// Bodies the two rejected credentials would otherwise be allowed to send:
	// the legacy token owns the plain-payload path, the docs token owns
	// DocsCard. Pairing each with target_role isolates the role gate from the
	// pre-existing payload-shape rules.
	cases := []struct {
		name  string
		token string
		body  map[string]interface{}
	}{
		{
			name:  "legacy NOTIFY_INTERNAL_TOKEN with a plain payload",
			token: roleLegacyToken,
			body: map[string]interface{}{
				"space_id": "sp_gate", "service": "whatever",
				"target_role": TargetRoleSpaceAdmin,
				"payload":     map[string]interface{}{"type": 1, "content": "who are the admins?"},
			},
		},
		{
			name:  "legacy NOTIFY_INTERNAL_TOKEN borrowing the approval card",
			token: roleLegacyToken,
			body:  approvalCardRoleRequest("sp_gate"),
		},
		{
			name:  "docs OCTO_DOCS_NOTIFY_TOKEN with a docs card",
			token: roleDocsToken,
			body: map[string]interface{}{
				"space_id": "sp_gate", "service": "docs",
				"target_role": TargetRoleSpaceAdmin,
				"docs_card":   validAccessRequestDocsCard(),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRoleTargetingRig(t)

			w := rig.post(t, tc.token, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "err.server.notify.card_not_allowed", decodeNotifyErrorCode(t, w))
			// The decisive assertion: no ActiveAdminUIDs query was registered on
			// the mock, so had the handler resolved the roster the DB layer would
			// have errored. The uids never left the database.
			assert.NoError(t, rig.mock.ExpectationsWereMet())
			assert.Zero(t, rig.sentCards(), "a rejected credential must not reach transport")
		})
	}

	t.Run("the action capability is admitted", func(t *testing.T) {
		rig := newRoleTargetingRig(t)
		rig.mock.ExpectQuery(`SELECT sm.uid`).
			WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("admin_uid"))
		rig.mock.ExpectQuery(`SELECT uid FROM space_member`).
			WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("admin_uid"))

		w := rig.post(t, roleActionToken, approvalCardRoleRequest("sp_gate_ok"))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Equal(t, []string{"admin_uid"}, decodeNotifyResp(t, w).Delivered)
		assert.NoError(t, rig.mock.ExpectationsWereMet())
	})
}

// Zero admins is a SUCCESS with an empty delivered list, not an error. An error
// would make the producer retry a state of the world that will not change on
// its own. Empty `filtered` alongside empty `delivered` is what distinguishes
// "no admins" from "everyone was filtered out".
func TestRoleTargeting_ZeroAdminsSucceedsWithEmptyDelivered(t *testing.T) {
	rig := newRoleTargetingRig(t)
	rig.mock.ExpectQuery(`SELECT sm.uid`).WillReturnRows(sqlmock.NewRows([]string{"uid"}))

	w := rig.post(t, roleActionToken, approvalCardRoleRequest("sp_no_admins"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	resp := decodeNotifyResp(t, w)
	assert.Empty(t, resp.Delivered)
	assert.Empty(t, resp.Filtered)
	assert.Contains(t, w.Body.String(), `"delivered":[]`,
		"an empty recipient set must serialize as [] so the consumer never decodes null")
	assert.Contains(t, w.Body.String(), `"filtered":{}`)
	assert.NoError(t, rig.mock.ExpectationsWereMet())
}

// Validation must not depend on tenant state. The zero-admin short-circuit is a
// 200, and it used to run BEFORE the per-path payload checks — so the same
// malformed request came back 400 from a Space that happens to have an admin and
// 200 from a Space that does not. That makes a contract violation undebuggable
// for the producer (it reports itself only sometimes) and leaks a bit about
// somebody else's Space to anyone able to compare the two responses.
//
// Every case here targets a Space with ZERO admins, which is precisely the state
// that used to mask the error. The ActiveAdminUIDs query is deliberately NOT
// registered on the mock: validation now rejects before resolution runs, and
// ExpectationsWereMet would surface a stray query.
func TestRoleTargeting_ValidationPrecedesRecipientResolution(t *testing.T) {
	withoutTitle := func(spaceID, title string) map[string]interface{} {
		b := approvalCardRoleRequest(spaceID)
		b["approval_card"].(map[string]interface{})["title"] = title
		return b
	}
	cases := []struct {
		name string
		body map[string]interface{}
		// wantCode is the expected error code, or "" when the rejection happens
		// in gin's binding layer (which renders a plain 400 with no OCTO code).
		// Either way the point of the test holds: 400 regardless of tenant state.
		wantCode string
		why      string
	}{
		{
			name:     "missing card title",
			body:     withoutTitle("sp_zero_admins", ""),
			wantCode: "err.server.notify.card_invalid",
			why:      "an empty Title is a caller contract violation regardless of who is an admin",
		},
		{
			name:     "whitespace-only card title",
			body:     withoutTitle("sp_zero_admins", "   "),
			wantCode: "err.server.notify.card_invalid",
		},
		{
			name:     "untrimmed space_id",
			body:     approvalCardRoleRequest(" sp_zero_admins "),
			wantCode: "err.server.notify.card_invalid",
			why: "spaceIDAcceptable rejects an untrimmed value rather than silently " +
				"delivering into a Space the caller did not name",
		},
		{
			name: "empty space_id",
			body: approvalCardRoleRequest(""),
			why:  "caught by NotifyReq's binding:\"required\", before the handler runs",
		},
		{
			name:     "whitespace-only space_id",
			body:     approvalCardRoleRequest("   "),
			wantCode: "err.server.notify.card_invalid",
		},
		{
			name: "too many custom actions",
			body: func() map[string]interface{} {
				b := approvalCardRoleRequest("sp_zero_admins")
				actions := make([]map[string]interface{}, 0, 6)
				for i := 0; i < 6; i++ {
					actions = append(actions, map[string]interface{}{
						"decision": "d" + itoa(i), "title": "Choice " + itoa(i),
					})
				}
				b["approval_card"].(map[string]interface{})["actions"] = actions
				return b
			}(),
			wantCode: "err.server.notify.card_invalid",
			why: "the card schema check lives in the rendered document, so it must " +
				"run ahead of resolution too — not after memberCache.verify",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRoleTargetingRig(t)

			w := rig.post(t, roleActionToken, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code,
				"a malformed request must be 400 even when the Space has zero admins "+
					"(%s); body: %s", tc.why, w.Body.String())
			if tc.wantCode != "" {
				assert.Equal(t, tc.wantCode, decodeNotifyErrorCode(t, w))
			}
			assert.NoError(t, rig.mock.ExpectationsWereMet(),
				"validation must reject before recipient resolution queries the DB")
			assert.Zero(t, rig.sentCards())
		})
	}

	// Control: the same well-formed body against the same zero-admin Space is a
	// 200. Without this the cases above would also pass if everything 400'd.
	t.Run("well-formed request against a zero-admin Space is 200", func(t *testing.T) {
		rig := newRoleTargetingRig(t)
		rig.mock.ExpectQuery(`SELECT sm.uid`).WillReturnRows(sqlmock.NewRows([]string{"uid"}))

		w := rig.post(t, roleActionToken, approvalCardRoleRequest("sp_zero_admins"))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Empty(t, decodeNotifyResp(t, w).Delivered)
		assert.NoError(t, rig.mock.ExpectationsWereMet())
	})
}

// The 200-recipient ceiling is respected. Unlike the explicit-targets path
// (where >200 is the caller's error), a role-resolved set is TRUNCATED: the
// producer does not choose how many admins a Space has.
func TestRoleTargeting_TruncatesAtTwoHundred(t *testing.T) {
	rig := newRoleTargetingRig(t)
	const spaceID = "sp_many_admins"
	rows := sqlmock.NewRows([]string{"uid"})
	all := roleAdminUIDs(maxNotifyTargets + 1)
	for _, uid := range all {
		rows.AddRow(uid)
	}
	rig.mock.ExpectQuery(`SELECT sm.uid`).WillReturnRows(rows)
	// Warm the member cache with every resolved uid so the delivery path does
	// not issue a second query; we only care about the truncation boundary.
	primeMemberCache(rig.n, spaceID, all...)

	w := rig.post(t, roleActionToken, approvalCardRoleRequest(spaceID))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Len(t, decodeNotifyResp(t, w).Delivered, maxNotifyTargets,
		"the role-resolved recipient set must be truncated at the 200 cap, "+
			"not rejected and not delivered in full")
	assert.NoError(t, rig.mock.ExpectationsWereMet())
}

// The actor is excluded BEFORE the cap, not after.
//
// The delivery path drops req.ActorUID, so truncating first spent one of the 200
// slots on a uid that was then thrown away: a Space with 201 admins whose
// notification was triggered by one of the first 200 got 199 deliveries, while
// an eligible 201st admin sat just past the cut and never learned about the
// review. The over-fetch of one row exists to absorb exactly this — there is at
// most one actor — so with the ordering fixed the delivered set stays full.
func TestRoleTargeting_ActorExcludedBeforeCap(t *testing.T) {
	rig := newRoleTargetingRig(t)
	const spaceID = "sp_actor_at_cap"

	all := roleAdminUIDs(maxNotifyTargets + 1)
	actorUID := all[0]
	rows := sqlmock.NewRows([]string{"uid"})
	for _, uid := range all {
		rows.AddRow(uid)
	}
	rig.mock.ExpectQuery(`SELECT sm.uid`).WillReturnRows(rows)
	primeMemberCache(rig.n, spaceID, all...)

	body := approvalCardRoleRequest(spaceID)
	body["actor_uid"] = actorUID
	w := rig.post(t, roleActionToken, body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	delivered := decodeNotifyResp(t, w).Delivered
	assert.Len(t, delivered, maxNotifyTargets,
		"with 201 admins and the actor among them, all 200 OTHER admins must be "+
			"delivered to; truncating before actor exclusion would deliver only 199")
	assert.NotContains(t, delivered, actorUID, "the actor never receives their own card")
	assert.NoError(t, rig.mock.ExpectationsWereMet())
}

// roleAdminUIDs builds n zero-padded, lexicographically ordered admin uids so
// the cap/actor boundary tests can name a specific position in the result set.
func roleAdminUIDs(n int) []string {
	uids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		uids = append(uids, "admin_"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i))
	}
	return uids
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Both / neither / unknown-role are rejected at the HTTP boundary with the
// shared param code, and — critically — with ZERO transport.
func TestRoleTargeting_InvalidTargetingRejectedWithoutDelivery(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "both targets and target_role",
			body: map[string]interface{}{
				"space_id": "sp_x", "service": "svc",
				"targets": []string{"u1"}, "target_role": TargetRoleSpaceAdmin,
				"payload": map[string]interface{}{"type": 1, "content": "x"},
			},
		},
		{
			name: "neither targets nor target_role",
			body: map[string]interface{}{
				"space_id": "sp_x", "service": "svc",
				"payload": map[string]interface{}{"type": 1, "content": "x"},
			},
		},
		{
			name: "unknown target_role",
			body: map[string]interface{}{
				"space_id": "sp_x", "service": "svc", "target_role": "everyone",
				"payload": map[string]interface{}{"type": 1, "content": "x"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			db, mock, closeDB := newMockedDBSession(t)
			defer closeDB()

			n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
			n.botOK.Store(true)

			w := doJSONRequest(t, newRoleTargetRouter(n), http.MethodPost, "/v1/internal/notify",
				http.Header{InternalTokenHeader: []string{"tk"}}, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "err.shared.param.invalid", decodeNotifyErrorCode(t, w))
			// No query ran and no message was sent.
			assert.NoError(t, mock.ExpectationsWereMet())
			assert.Zero(t, wk.messageCount, "an invalid targeting request must not deliver anything")
		})
	}
}

// /notify/batch does not accept target_role: a batch carrying it anywhere is
// rejected whole, before ANY earlier text entry is delivered. The raw value is
// compared, so a whitespace-only "   " is a producer bug rather than "unset".
func TestRoleTargeting_BatchRejectsRoleWholeBatch(t *testing.T) {
	cases := []struct {
		name          string
		notifications []map[string]interface{}
	}{
		{
			name: "entry carries target_role",
			notifications: []map[string]interface{}{
				{"space_id": "sp_x", "service": "svc", "targets": []string{"u1"},
					"payload": map[string]interface{}{"type": 1, "content": "a"}},
				{"space_id": "sp_x", "service": "svc", "target_role": TargetRoleSpaceAdmin,
					"payload": map[string]interface{}{"type": 1, "content": "b"}},
			},
		},
		{
			name: "entry carries an unknown target_role",
			notifications: []map[string]interface{}{
				{"space_id": "sp_x", "service": "svc", "targets": []string{"u1"},
					"payload": map[string]interface{}{"type": 1, "content": "a"}},
				{"space_id": "sp_x", "service": "svc", "target_role": "everyone",
					"targets": []string{"u1"},
					"payload": map[string]interface{}{"type": 1, "content": "b"}},
			},
		},
		{
			// The nit from review: strings.TrimSpace would classify this as
			// "unset" and let the batch through. The rule is "carries the field
			// at all", so it must be compared before trimming.
			name: "entry carries a whitespace-only target_role",
			notifications: []map[string]interface{}{
				{"space_id": "sp_x", "service": "svc", "targets": []string{"u1"},
					"payload": map[string]interface{}{"type": 1, "content": "a"}},
				{"space_id": "sp_x", "service": "svc", "target_role": "   ",
					"targets": []string{"u1"},
					"payload": map[string]interface{}{"type": 1, "content": "b"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			db, mock, closeDB := newMockedDBSession(t)
			defer closeDB()

			n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
			n.botOK.Store(true)
			primeMemberCache(n, "sp_x", "u1")

			w := doJSONRequest(t, newRoleTargetRouter(n), http.MethodPost,
				"/v1/internal/notify/batch",
				http.Header{InternalTokenHeader: []string{"tk"}},
				map[string]interface{}{"notifications": tc.notifications})

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, "err.shared.param.invalid", decodeNotifyErrorCode(t, w))
			assert.Zero(t, wk.messageCount,
				"the batch preflight must reject before delivering ANY earlier entry")
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// A batch entry with no usable `targets` stays a PER-ITEM error inside a 207,
// with the batch's other entries still delivered.
//
// This pins the pre-existing wire contract, which an earlier revision of the
// preflight silently converted into a whole-batch 400 that delivered nothing —
// justified by a comment claiming NotifyReq.Targets used to carry
// binding:"required" and that the preflight merely restored it. Verified
// against this repo's validator (v10.14.0, TagName("binding")), that claim is
// wrong on both counts:
//
//	body={"targets":[]}     nil=false len=0 required_err=false   <- [] PASSED `required`
//	BatchNotifyReq.Notifications has no `dive`, so NotifyReq's own binding tags
//	were never applied to batch entries in the first place.
//
// So both shapes below bound cleanly at every version of this handler and came
// back as BatchNotifyResult.Error inside a 207. `has_errors` plus a per-entry
// `error` string is exactly the channel this endpoint has always used to report
// one bad entry without punishing the other 49.
func TestBatch_EntryWithoutTargetsStaysPerItem207(t *testing.T) {
	cases := []struct {
		name string
		bad  map[string]interface{}
	}{
		{
			name: "explicit empty targets array",
			bad: map[string]interface{}{"space_id": "sp_x", "service": "svc",
				"targets": []string{},
				"payload": map[string]interface{}{"type": 1, "content": "b"}},
		},
		{
			name: "targets key absent entirely",
			bad: map[string]interface{}{"space_id": "sp_x", "service": "svc",
				"payload": map[string]interface{}{"type": 1, "content": "b"}},
		},
		{
			name: "explicit null targets",
			bad: map[string]interface{}{"space_id": "sp_x", "service": "svc",
				"targets": nil,
				"payload": map[string]interface{}{"type": 1, "content": "b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			db, mock, closeDB := newMockedDBSession(t)
			defer closeDB()

			n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
			n.botOK.Store(true)
			primeMemberCache(n, "sp_x", "u1")

			w := doJSONRequest(t, newRoleTargetRouter(n), http.MethodPost,
				"/v1/internal/notify/batch",
				http.Header{InternalTokenHeader: []string{"tk"}},
				map[string]interface{}{"notifications": []map[string]interface{}{
					{"space_id": "sp_x", "service": "svc", "targets": []string{"u1"},
						"payload": map[string]interface{}{"type": 1, "content": "a"}},
					tc.bad,
				}})

			require.Equal(t, http.StatusMultiStatus, w.Code,
				"one malformed entry must not cancel the whole batch; body: %s", w.Body.String())

			var resp BatchNotifyResp
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
			require.Len(t, resp.Results, 2)
			assert.True(t, resp.HasErrors)
			assert.Equal(t, []string{"u1"}, resp.Results[0].Delivered,
				"the well-formed entry must still be delivered")
			assert.Empty(t, resp.Results[0].Error)
			assert.NotEmpty(t, resp.Results[1].Error,
				"the malformed entry is reported per-item, which is what "+
					"BatchNotifyResult.Error exists for")
			assert.Empty(t, resp.Results[1].Delivered)
			assert.Equal(t, int32(1), atomic.LoadInt32(&wk.messageCount),
				"exactly the one deliverable entry reached transport")
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// Backward-compatibility control: a plain `targets` request behaves exactly as
// before — no extra query, same delivered/filtered semantics. This is the shape
// docs-backend, bot-mention and the summary-card pilot all send.
func TestRoleTargeting_ExplicitTargetsPathUnchanged(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, mock, closeDB := newMockedDBSession(t)
	defer closeDB()

	const spaceID = "sp_bc"
	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "u1")

	w := doJSONRequest(t, newRoleTargetRouter(n), http.MethodPost, "/v1/internal/notify",
		http.Header{InternalTokenHeader: []string{"tk"}}, map[string]interface{}{
			"space_id": spaceID,
			"service":  "docs-service",
			"targets":  []string{"u1", "u2"},
			"payload":  map[string]interface{}{"type": 1, "content": "hi"},
		})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	resp := decodeNotifyResp(t, w)
	assert.ElementsMatch(t, []string{"u1"}, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["u2"])
	// No ActiveAdminUIDs query was registered, so any attempt to run one would
	// have surfaced here.
	assert.NoError(t, mock.ExpectationsWereMet())
}

// The `target_role` key must be absent from a serialized request that does not
// set it, so a producer round-tripping NotifyReq cannot accidentally start
// sending it.
func TestNotifyReqTargetRoleIsOmittedWhenUnset(t *testing.T) {
	raw, err := json.Marshal(NotifyReq{SpaceID: "sp", Service: "svc", Targets: []string{"u"}})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "target_role")
}

// The role vocabulary is shared with modules/space's query semantics; pin the
// wire value so a rename cannot silently break the marketplace client.
func TestTargetRoleSpaceAdminWireValue(t *testing.T) {
	assert.Equal(t, "space_admin", TargetRoleSpaceAdmin)
}

// =============================================================================
// Fixed internal-token exclusion set (cross-module symmetry)
// =============================================================================

const tokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 32 bytes
const tokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// The one foreign fixed internal-token env this change introduces
// (OCTO_MARKETPLACE_INTERNAL_TOKEN) must disable a colliding notify token.
// modules/space runs the mirror-image comparison, so a shared value fails BOTH
// capabilities closed rather than picking an arbitrary winner.
func TestResolveInternalTokens_MarketplaceCollisionDisablesNotifyTokens(t *testing.T) {
	foreign := marketplaceInternalTokenEnvForExclusion

	t.Run("legacy vs "+foreign, func(t *testing.T) {
		getenv := func(k string) string {
			switch k {
			case notifyInternalTokenEnv, foreign:
				return tokenA
			case docsNotifyInternalTokenEnv:
				return tokenB
			}
			return ""
		}
		token, docsToken, _, bootErrors := resolveInternalTokens(getenv)
		assert.Empty(t, token,
			"NOTIFY_INTERNAL_TOKEN colliding with %s must disable the legacy capability", foreign)
		assert.Equal(t, tokenB, docsToken, "the non-colliding sibling must survive")
		require.NotEmpty(t, bootErrors)
		assert.Contains(t, strings.Join(bootErrors, "\n"), foreign)
		for _, e := range bootErrors {
			assert.NotContains(t, e, tokenA, "boot errors must never contain token values")
		}
	})

	t.Run("docs vs "+foreign, func(t *testing.T) {
		getenv := func(k string) string {
			switch k {
			case docsNotifyInternalTokenEnv, foreign:
				return tokenA
			case notifyInternalTokenEnv:
				return tokenB
			}
			return ""
		}
		token, docsToken, _, bootErrors := resolveInternalTokens(getenv)
		assert.Equal(t, tokenB, token)
		assert.Empty(t, docsToken,
			"OCTO_DOCS_NOTIFY_TOKEN colliding with %s must disable the docs capability", foreign)
		require.NotEmpty(t, bootErrors)
	})
}

// The pre-existing fixed internal-token envs owned by other modules are NOT in
// this module's exclusion set. Those pairs predate this change and a deployment
// running one of them today keeps working: narrowing an existing credential's
// behaviour is out of scope for adding the marketplace token.
func TestResolveInternalTokens_PreExistingForeignEnvsAreNotExcluded(t *testing.T) {
	for _, foreign := range []string{"OCTO_DOCS_BOT_MENTION_TOKEN", "OCTO_DRIVE_INTERNAL_TOKEN"} {
		t.Run(foreign, func(t *testing.T) {
			getenv := func(k string) string {
				switch k {
				case notifyInternalTokenEnv, foreign:
					return tokenA
				case docsNotifyInternalTokenEnv:
					return tokenB
				}
				return ""
			}
			token, _, _, bootErrors := resolveInternalTokens(getenv)
			assert.Equal(t, tokenA, token,
				"%s is a pre-existing pair; this change must not start disabling it", foreign)
			assert.Empty(t, bootErrors)
		})
	}
}

// The marketplace env spelling must track modules/space's exported constant.
// The literal is duplicated on purpose (no production import); this pins it.
func TestForeignTokenEnvSpellingsMatchOwningPackages(t *testing.T) {
	assert.Equal(t, space.MarketplaceInternalTokenEnv, marketplaceInternalTokenEnvForExclusion,
		"modules/space renamed its token env; update the literal in modules/notify/config.go")
}

// The pre-existing intra-module tie-break is preserved verbatim: legacy wins,
// docs is disabled, the process keeps booting. This change deliberately does
// not make that pair fatal — see the file comment in config.go.
func TestResolveInternalTokens_IntraModuleCollisionKeepsLegacy(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case notifyInternalTokenEnv, docsNotifyInternalTokenEnv:
			return tokenA
		}
		return ""
	}
	token, docsToken, _, bootErrors := resolveInternalTokens(getenv)
	assert.Equal(t, tokenA, token)
	assert.Empty(t, docsToken)
	require.NotEmpty(t, bootErrors)
}

// Two unset envs must not be reported as "colliding" with each other.
func TestResolveInternalTokens_UnsetIsWarnedNotErrored(t *testing.T) {
	token, docsToken, warnings, bootErrors := resolveInternalTokens(func(string) string { return "" })
	assert.Empty(t, token)
	assert.Empty(t, docsToken)
	assert.Len(t, warnings, 2)
	assert.Empty(t, bootErrors, "unset envs are a warning, never a collision error")
}

// The value handed to internalAuthMiddleware is the RAW env value.
//
// NOTIFY_INTERNAL_TOKEN and OCTO_DOCS_NOTIFY_TOKEN both predate this change.
// Trimming them at load time would silently redefine what those two deployed
// credentials accept: an operator with a padded value whose client sends the
// byte-exact configured string would start getting 401. Nothing in this file
// normalizes, so authentication stays byte-exact and the collision comparison
// matches the sibling modules, which also compare raw.
func TestResolveInternalTokens_TokenIsNotTrimmedForAuthentication(t *testing.T) {
	padded := "  " + tokenA + "\n"
	getenv := func(k string) string {
		switch k {
		case notifyInternalTokenEnv:
			return padded
		case docsNotifyInternalTokenEnv:
			return tokenB
		}
		return ""
	}
	token, docsToken, _, bootErrors := resolveInternalTokens(getenv)
	assert.Equal(t, padded, token,
		"the credential compared against the request header must be the env value "+
			"byte for byte, not a normalized form of it")
	assert.Equal(t, tokenB, docsToken)
	assert.Empty(t, bootErrors)
}

func TestResolveInternalTokens_CleanConfigPassesThrough(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case notifyInternalTokenEnv:
			return tokenA
		case docsNotifyInternalTokenEnv:
			return tokenB
		}
		return ""
	}
	token, docsToken, warnings, bootErrors := resolveInternalTokens(getenv)
	assert.Equal(t, tokenA, token)
	assert.Equal(t, tokenB, docsToken)
	assert.Empty(t, warnings)
	assert.Empty(t, bootErrors)
}

func TestResolveInternalTokens_NilGetenvFailsClosed(t *testing.T) {
	token, docsToken, _, bootErrors := resolveInternalTokens(nil)
	assert.Empty(t, token)
	assert.Empty(t, docsToken)
	assert.NotEmpty(t, bootErrors)
}
