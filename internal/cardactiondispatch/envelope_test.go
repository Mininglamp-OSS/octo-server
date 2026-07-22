package cardactiondispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMarshalCallbackRequestSupportsLegacyAndOctoCardEnvelope(t *testing.T) {
	event := Event{
		EventID: 9007199254740993, SenderUID: "notification", Owner: "docs", ActionType: "access_request.decision",
		MessageID: "1001", ChannelID: "user-b", ChannelType: 1, SpaceID: "space-1",
		ActionID: "docs-access-approve", OperatorUID: "reviewer-1", ActedAt: 123,
		Inputs: map[string]any{}, Data: map[string]any{"owner": "docs", "action_type": "access_request.decision", "decision": "approve"},
		Card: CardContext{TemplateID: "docs.access-request", TemplateVersion: "0.2.0", View: "pending"},
	}
	legacy, err := MarshalCallbackRequest(event, CallbackFormatLegacy)
	if err != nil {
		t.Fatalf("MarshalCallbackRequest(legacy): %v", err)
	}
	wantLegacy := `{"event_id":"9007199254740993","action_id":"docs-access-approve","decision":"approve","operator_uid":"reviewer-1","inputs":{},"data":{"action_type":"access_request.decision","decision":"approve","owner":"docs"},"message_id":"1001","channel_id":"user-b","channel_type":1,"space_id":"space-1","acted_at":123}`
	if string(legacy) != wantLegacy {
		t.Fatalf("legacy body drifted:\n got %s\nwant %s", legacy, wantLegacy)
	}
	legacyEvent := event
	legacyEvent.Card = CardContext{}
	legacyOnUpgradedRoute, err := marshalDecisionRequest(DecisionRequestFromEvent(legacyEvent), CallbackFormatOctoCardV1)
	if err != nil {
		t.Fatalf("legacy card on upgraded route: %v", err)
	}
	if string(legacyOnUpgradedRoute) != wantLegacy {
		t.Fatalf("legacy card compatibility body drifted:\n got %s\nwant %s", legacyOnUpgradedRoute, wantLegacy)
	}
	partialEvent := legacyEvent
	partialEvent.Card.TemplateID = "docs.access-request"
	if _, err := marshalDecisionRequest(DecisionRequestFromEvent(partialEvent), CallbackFormatOctoCardV1); err == nil {
		t.Fatal("partial callback CardContext must fail closed")
	}

	standard, err := MarshalCallbackRequest(event, CallbackFormatOctoCardV1)
	if err != nil {
		t.Fatalf("MarshalCallbackRequest(octo-card-v1): %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(standard, &body); err != nil {
		t.Fatalf("decode standard body: %v", err)
	}
	if body["protocol"] != "octo-card@1.0" || body["type"] != "card.action" || body["trigger_id"] != "9007199254740993" {
		t.Fatalf("standard envelope header = %+v", body)
	}
	action := body["action"].(map[string]any)
	if action["id"] != event.ActionID {
		t.Fatalf("action = %+v", action)
	}
	card := body["card"].(map[string]any)
	if card["template_id"] != event.Card.TemplateID || card["template_version"] != event.Card.TemplateVersion || card["view"] != "pending" {
		t.Fatalf("card context = %+v", card)
	}
	actor := body["actor"].(map[string]any)
	if actor["uid"] != event.OperatorUID {
		t.Fatalf("actor = %+v", actor)
	}
	if _, exists := body["response_url"]; exists {
		t.Fatal("response_url must not be emitted before a signed response contract exists")
	}
}

func TestLoadRouteSpecsCallbackFormatDefaultsLegacyAndRejectsUnknown(t *testing.T) {
	specs, err := LoadRouteSpecs(`[{
		"sender_uid":"notification","owner":"docs","action_type":"access_request.decision",
		"url":"https://docs.internal/callback","secret_env":"SECRET"
	}]`)
	if err != nil {
		t.Fatalf("LoadRouteSpecs(default): %v", err)
	}
	if len(specs) != 1 || specs[0].CallbackFormat != CallbackFormatLegacy {
		t.Fatalf("default callback format = %+v", specs)
	}
	specs, err = LoadRouteSpecs(`[{
		"sender_uid":"notification","owner":"docs","action_type":"access_request.decision",
		"url":"https://docs.internal/callback","secret_env":"SECRET","callback_format":"octo-card-v1"
	}]`)
	if err != nil || specs[0].CallbackFormat != CallbackFormatOctoCardV1 {
		t.Fatalf("LoadRouteSpecs(octo-card-v1) = (%+v, %v)", specs, err)
	}
	if _, err := LoadRouteSpecs(`[{
		"sender_uid":"notification","owner":"docs","action_type":"access_request.decision",
		"url":"https://docs.internal/callback","secret_env":"SECRET","callback_format":"future"
	}]`); err == nil {
		t.Fatal("LoadRouteSpecs(unknown callback format) error = nil")
	}
}

func TestHTTPDelivererSignsExactOctoCardEnvelope(t *testing.T) {
	var signatureOK atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read callback body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if Verify(testCallbackSecret, r.Header.Get(HeaderSignature), r.Method, r.URL.EscapedPath(),
			r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderEventID), raw) {
			signatureOK.Store(true)
		}
		var envelope map[string]any
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope["protocol"] != "octo-card@1.0" {
			t.Errorf("octo-card callback body = %s, error = %v", raw, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disposition":"applied","state":"approved","requester_uid":"user-a"}`))
	}))
	defer server.Close()

	event := testDispatchEvent()
	event.ActionID = "docs-access-approve"
	event.Card = CardContext{TemplateID: "docs.access-request", TemplateVersion: "0.3.0", View: "pending"}
	route := &Route{
		URL: server.URL + "/v1/card-actions/decide", Timeout: time.Second,
		CallbackFormat: CallbackFormatOctoCardV1, secret: testCallbackSecret,
	}
	deliverer := NewHTTPDeliverer(server.Client().Transport, func() time.Time {
		return time.Unix(1_784_073_600, 0)
	})
	if _, err := deliverer.Deliver(context.Background(), route, DecisionRequestFromEvent(event)); err != nil {
		t.Fatalf("Deliver(octo-card-v1): %v", err)
	}
	if !signatureOK.Load() {
		t.Fatal("octo-card callback signature did not verify against the exact body")
	}
}
