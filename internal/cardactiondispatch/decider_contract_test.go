package cardactiondispatch

import (
	"encoding/json"
	"strings"
	"testing"
)

// The operator's trusted current Space rides the callback as operator_space_id,
// kept distinct from space_id (the card's authoritative origin Space).
func TestDecisionRequestCarriesOperatorSpaceIDDistinctFromCardSpace(t *testing.T) {
	body, err := json.Marshal(DecisionRequestFromEvent(Event{
		EventID: 7, SpaceID: "card-origin-space", OperatorSpaceID: "operator-current-space",
	}))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got := string(fields["space_id"]); got != `"card-origin-space"` {
		t.Fatalf("space_id JSON = %s, want the card origin Space", got)
	}
	if got := string(fields["operator_space_id"]); got != `"operator-current-space"` {
		t.Fatalf("operator_space_id JSON = %s, want the operator's current Space", got)
	}
}

// An empty operator Space is omitted rather than sent as "".
func TestDecisionRequestOmitsEmptyOperatorSpaceID(t *testing.T) {
	body, err := json.Marshal(DecisionRequestFromEvent(Event{EventID: 1}))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(body), "operator_space_id") {
		t.Fatalf("empty operator_space_id was emitted: %s", body)
	}
}

// The octo-card-v1 envelope carries the same trusted operator Space inside the
// authenticated actor object; it must not be lost when a route opts into the
// structured callback format.
func TestOctoCardEnvelopeCarriesOperatorSpaceID(t *testing.T) {
	body, err := MarshalCallbackRequest(Event{
		EventID: 9, OperatorUID: "operator-1", OperatorSpaceID: "operator-space",
		ActionID: "approve", Card: CardContext{TemplateID: "t", TemplateVersion: "1", View: "pending"},
	}, CallbackFormatOctoCardV1)
	if err != nil {
		t.Fatalf("MarshalCallbackRequest() error = %v", err)
	}
	var envelope struct {
		Actor struct {
			UID     string `json:"uid"`
			SpaceID string `json:"space_id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if envelope.Actor.UID != "operator-1" || envelope.Actor.SpaceID != "operator-space" {
		t.Fatalf("actor = %+v", envelope.Actor)
	}
}

func TestDecodeDecisionResultAcceptsAuthoritativeDeciderFields(t *testing.T) {
	valid := `{"disposition":"applied","state":"approved","requester_uid":"user-a",` +
		`"decider_uid":"decider-1","decider_space_id":"space-op","decided_at":1787223660}`
	result, err := DecodeDecisionResult(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("DecodeDecisionResult(valid) error = %v", err)
	}
	if result.DeciderUID != "decider-1" || result.DeciderSpaceID != "space-op" || result.DecidedAt != 1787223660 {
		t.Fatalf("DecodeDecisionResult authoritative fields = %+v", result)
	}
}

func TestDecodeDecisionResultRejectsMalformedDeciderFields(t *testing.T) {
	invalid := map[string]string{
		"oversized decider_uid": `{"disposition":"applied","state":"approved","requester_uid":"u",` +
			`"decider_uid":"` + strings.Repeat("x", 129) + `"}`,
		"untrimmed decider_uid": `{"disposition":"applied","state":"approved","requester_uid":"u",` +
			`"decider_uid":" decider "}`,
		"oversized decider_space_id": `{"disposition":"applied","state":"approved","requester_uid":"u",` +
			`"decider_uid":"d","decider_space_id":"` + strings.Repeat("s", 129) + `"}`,
		"space without decider": `{"disposition":"applied","state":"approved","requester_uid":"u",` +
			`"decider_space_id":"space-op"}`,
		"string decided_at":   `{"disposition":"applied","state":"approved","requester_uid":"u","decided_at":"2026"}`,
		"negative decided_at": `{"disposition":"applied","state":"approved","requester_uid":"u","decided_at":-1}`,
	}
	for name, body := range invalid {
		if _, err := DecodeDecisionResult(strings.NewReader(body)); err == nil {
			t.Errorf("DecodeDecisionResult(%s) error = nil, want rejection", name)
		}
	}
}

func TestValidateAuthoritativeDeciderIDsUsesByteBounds(t *testing.T) {
	// 42 three-byte runes plus two ASCII bytes are exactly 128 bytes and
	// accepted; adding another three-byte rune exceeds the contract even though
	// both strings are well under 128 characters.
	if err := ValidateAuthoritativeDeciderIDs(strings.Repeat("界", 42)+"ab", "space"); err != nil {
		t.Fatalf("128-byte decider_uid rejected: %v", err)
	}
	if err := ValidateAuthoritativeDeciderIDs(strings.Repeat("界", 43), "space"); err == nil {
		t.Fatal("129-byte decider_uid accepted")
	}
}
