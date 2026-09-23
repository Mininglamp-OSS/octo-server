package bot_api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

type denyingGenericStore struct{ *fakeOBOStore }

func (s *denyingGenericStore) putDelegationAtomic(context.Context, string, string, bool, bool, bool, optionalExpiry) (*genericDelegationView, error) {
	return nil, errGenericBotNotOwned
}

func (s *denyingGenericStore) setGenericBinding(context.Context, string, int64, bool) (*genericDelegationView, error) {
	return nil, errGenericBotNotOwned
}

func (s *denyingGenericStore) genericViewForOwner(string, int64) (*genericDelegationView, error) {
	return nil, errGenericBotNotOwned
}

func (s *denyingGenericStore) listGenericAudits(string, int64) ([]genericAuditRow, error) {
	return nil, errGenericBotNotOwned
}

func TestOptionalExpiryDistinguishesAbsentNullAndTimestamp(t *testing.T) {
	for _, tc := range []struct {
		body     string
		set      bool
		nilValue bool
		valid    bool
	}{
		{`{}`, false, true, true},
		{`{"expires_at":null}`, true, true, true},
		{`{"expires_at":"2026-10-01T12:00:00+08:00"}`, true, false, true},
		{`{"expires_at":"2026-10-01 12:00:00"}`, true, true, false},
	} {
		var req putDelegationRequest
		err := json.Unmarshal([]byte(tc.body), &req)
		if (err == nil) != tc.valid {
			t.Fatalf("body=%s err=%v", tc.body, err)
		}
		if err == nil && (req.ExpiresAt.Set != tc.set || (req.ExpiresAt.Value == nil) != tc.nilValue) {
			t.Fatalf("body=%s expiry=%+v", tc.body, req.ExpiresAt)
		}
		if err == nil && req.ExpiresAt.Value != nil && req.ExpiresAt.Value.Location().String() != "UTC" {
			t.Fatalf("expiry not normalized to UTC: %v", req.ExpiresAt.Value)
		}
	}
}

func TestPolicyGrantModeSeparatesDeprecatedPersonaRuntime(t *testing.T) {
	if predicate := usableGrantPredicate(""); !strings.Contains(predicate, "mode<>'policy'") {
		t.Fatalf("legacy predicate must exclude policy grants: %s", predicate)
	}
	if !strings.Contains(genericGrantForOwnerSQL, "g.mode='policy'") {
		t.Fatalf("policy management must require its reserved mode: %s", genericGrantForOwnerSQL)
	}
}

func TestGenericGrantEffectiveActiveIncludesExpiry(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	if !genericGrantActiveAt(&genericGrantRow{Active: 1, ExpiresAt: &future}, now) {
		t.Fatal("unexpired active Grant reported inactive")
	}
	if genericGrantActiveAt(&genericGrantRow{Active: 1, ExpiresAt: &past}, now) {
		t.Fatal("expired Grant reported active")
	}
	if genericGrantActiveAt(&genericGrantRow{Active: 0}, now) {
		t.Fatal("paused Grant reported active")
	}
}

func TestGenericManagementReadsArePrivateNoStore(t *testing.T) {
	ba := &BotAPI{}
	for name, handler := range map[string]func(*wkhttp.Context){
		"grant":    ba.oboGetGenericGrant,
		"bindings": ba.oboListGenericBindings,
		"audits":   ba.oboListPolicyAudits,
	} {
		t.Run(name, func(t *testing.T) {
			c, recorder := makeRawCtx(t, "human-1", http.MethodGet, "/v1/obo/grants/bad", nil,
				gin.Params{{Key: "id", Value: "bad"}})
			handler(c)
			if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("Cache-Control=%q", got)
			}
		})
	}
}

func TestGenericManagementReadUsesStoreAndHidesNonOwner(t *testing.T) {
	ba := &BotAPI{oboStoreOverride: &denyingGenericStore{fakeOBOStore: newFakeOBOStore()}}
	c, recorder := makeRawCtx(t, "human-2", http.MethodGet, "/v1/obo/grants/7", nil,
		gin.Params{{Key: "id", Value: "7"}})
	ba.oboGetGenericGrant(c)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestNormalizeGenericScopes(t *testing.T) {
	for _, tc := range []struct {
		input []string
		all   bool
		ok    bool
	}{
		{[]string{}, false, true},
		{[]string{"ALL"}, true, true},
		{nil, false, false},
		{[]string{"ALL", "ALL"}, false, false},
		{[]string{"PROJECT"}, false, false},
	} {
		all, err := normalizeGenericScopes(tc.input)
		if all != tc.all || (err == nil) != tc.ok {
			t.Fatalf("scopes=%v got all=%v err=%v", tc.input, all, err)
		}
	}
}

func TestGenericManagementJSONRejectsSubjectSelectionAndTrailingData(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"active":true,"global_enabled":true,"scope_codes":["ALL"]}`, true},
		{`{"active":true,"global_enabled":true,"scope_codes":["ALL"],"grantor_uid":"other"}`, false},
		{`{"active":true} {"active":false}`, false},
	} {
		c, _ := makeRawCtx(t, "human-1", http.MethodPut, "/v1/obo/delegations/bot-1", []byte(tc.body), gin.Params{})
		var req putDelegationRequest
		err := bindGenericManagementJSON(c, &req)
		if (err == nil) != tc.valid {
			t.Fatalf("body=%s valid=%v err=%v", tc.body, tc.valid, err)
		}
	}
}
