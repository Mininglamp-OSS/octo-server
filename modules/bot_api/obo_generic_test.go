package bot_api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

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
