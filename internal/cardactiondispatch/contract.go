package cardactiondispatch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const MaxDecisionResponseBytes = 64 << 10

type Disposition string

const (
	DispositionApplied   Disposition = "applied"
	DispositionReplayed  Disposition = "replayed"
	DispositionForbidden Disposition = "forbidden"
	DispositionConflict  Disposition = "conflict"
	DispositionNotFound  Disposition = "not_found"
)

type State string

const (
	StatePending   State = "pending"
	StateApproved  State = "approved"
	StateDenied    State = "denied"
	StateCancelled State = "cancelled"
)

type Event struct {
	EventID     int64  `json:"event_id"`
	SenderUID   string `json:"sender_uid"`
	Owner       string `json:"owner"`
	ActionType  string `json:"action_type"`
	MessageID   string `json:"message_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	SpaceID     string `json:"space_id,omitempty"`
	ActionID    string `json:"action_id"`
	OperatorUID string `json:"operator_uid"`
	// OperatorSpaceID is the operator's TRUSTED current Space at click time —
	// the value SpaceMiddleware validated and pinned in the request context. It
	// is captured separately from SpaceID (the card/document authoritative origin
	// Space) and never relabels it: the two answer different questions and can
	// differ when the operator acts on a card that originates in another of their
	// Spaces. Empty when the click carried no server-verified Space.
	OperatorSpaceID string                 `json:"operator_space_id,omitempty"`
	ClientToken     string                 `json:"client_token,omitempty"`
	ActedAt         int64                  `json:"acted_at"`
	Inputs          map[string]interface{} `json:"inputs"`
	Data            map[string]interface{} `json:"data,omitempty"`
	Card            CardContext            `json:"card,omitempty"`
}

// CardContext is derived from the effective server-authored card frame before
// enqueue. Zero value means a legacy card without registry metadata.
//
// PrincipalType/PrincipalID/SpaceID are additive PR-C D3 fields: the action
// ingress copies them from the frame's validated `catalog_provenance` marker
// after proving consistency with the stored sender and authoritative Space.
// They are absent on frames sent before provenance existed; consumers must
// not fall back to guessing a principal from sender/owner when they are set.
type CardContext struct {
	TemplateID      string `json:"template_id,omitempty"`
	TemplateVersion string `json:"template_version,omitempty"`
	View            string `json:"view,omitempty"`
	PrincipalType   string `json:"principal_type,omitempty"`
	PrincipalID     string `json:"principal_id,omitempty"`
	SpaceID         string `json:"space_id,omitempty"`
}

type CallbackFormat string

const (
	CallbackFormatLegacy     CallbackFormat = "legacy"
	CallbackFormatOctoCardV1 CallbackFormat = "octo-card-v1"
)

type DecisionRequest struct {
	EventID     int64                  `json:"event_id,string"`
	ActionID    string                 `json:"action_id"`
	Decision    string                 `json:"decision"`
	OperatorUID string                 `json:"operator_uid"`
	DocID       string                 `json:"doc_id,omitempty"`
	RequestID   string                 `json:"request_id,omitempty"`
	Inputs      map[string]interface{} `json:"inputs"`
	Data        map[string]interface{} `json:"data,omitempty"`
	MessageID   string                 `json:"message_id"`
	ChannelID   string                 `json:"channel_id"`
	ChannelType uint8                  `json:"channel_type"`
	SpaceID     string                 `json:"space_id,omitempty"`
	// OperatorSpaceID carries the operator's trusted current Space to the
	// decision consumer alongside SpaceID (the card's origin Space), so the
	// consumer can attribute the click without conflating the two.
	OperatorSpaceID string `json:"operator_space_id,omitempty"`
	ActedAt         int64  `json:"acted_at"`
	event           *Event
}

type DecisionResult struct {
	Disposition  Disposition `json:"disposition"`
	State        State       `json:"state"`
	RequesterUID string      `json:"requester_uid,omitempty"`
	// Authoritative decider identity. Docs is the source of truth for WHO
	// decided; for a duplicate/concurrent click that is the FIRST decider, not
	// the current clicker, so these win over the clicker for display. octo-server
	// resolves the decider's display name and operator Space name from these ids
	// internally (user service + active-membership check) — a caller never sends
	// display copy that overrides them. DecidedAt is the authoritative decision
	// timestamp/label produced by Docs.
	DeciderUID     string            `json:"decider_uid,omitempty"`
	DeciderSpaceID string            `json:"decider_space_id,omitempty"`
	DecidedAt      int64             `json:"decided_at,omitempty"`
	Display        map[string]string `json:"display,omitempty"`
}

// ValidateAuthoritativeDeciderIDs validates the optional identity pair returned
// by a decision consumer. IDs use byte-length bounds, matching Go's len(string)
// semantics elsewhere in this wire contract. Empty values are allowed for
// rolling compatibility, but a Space cannot be attributed without a decider.
func ValidateAuthoritativeDeciderIDs(deciderUID, deciderSpaceID string) error {
	if len(deciderUID) > 128 || strings.TrimSpace(deciderUID) != deciderUID {
		return errors.New("cardactiondispatch: invalid decider_uid")
	}
	if len(deciderSpaceID) > 128 || strings.TrimSpace(deciderSpaceID) != deciderSpaceID {
		return errors.New("cardactiondispatch: invalid decider_space_id")
	}
	if deciderSpaceID != "" && deciderUID == "" {
		return errors.New("cardactiondispatch: decider_space_id requires decider_uid")
	}
	return nil
}

func DecisionRequestFromEvent(event Event) DecisionRequest {
	inputs := event.Inputs
	if inputs == nil {
		inputs = map[string]interface{}{}
	}
	request := DecisionRequest{
		EventID:         event.EventID,
		ActionID:        event.ActionID,
		Decision:        stringField(event.Data, "decision"),
		OperatorUID:     event.OperatorUID,
		DocID:           stringField(event.Data, "doc_id"),
		RequestID:       stringField(event.Data, "request_id"),
		Inputs:          inputs,
		Data:            event.Data,
		MessageID:       event.MessageID,
		ChannelID:       event.ChannelID,
		ChannelType:     event.ChannelType,
		SpaceID:         event.SpaceID,
		OperatorSpaceID: event.OperatorSpaceID,
		ActedAt:         event.ActedAt,
	}
	request.event = &event
	return request
}

type callbackEnvelope struct {
	Protocol  string         `json:"protocol"`
	Type      string         `json:"type"`
	Action    callbackAction `json:"action"`
	Card      callbackCard   `json:"card"`
	Actor     callbackActor  `json:"actor"`
	TriggerID string         `json:"trigger_id"`
}

type callbackAction struct {
	ID     string                 `json:"id"`
	Data   map[string]interface{} `json:"data,omitempty"`
	Inputs map[string]interface{} `json:"inputs"`
}

type callbackCard struct {
	TemplateID      string `json:"template_id"`
	TemplateVersion string `json:"template_version"`
	View            string `json:"view"`
	MessageID       string `json:"message_id"`
	ChannelID       string `json:"channel_id"`
	ChannelType     uint8  `json:"channel_type"`
}

type callbackActor struct {
	UID     string `json:"uid"`
	SpaceID string `json:"space_id,omitempty"`
}

// MarshalCallbackRequest encodes either the frozen flat request or the
// route-opted-in octo-card envelope. response_url is intentionally absent until
// a signed inbound response contract exists.
func MarshalCallbackRequest(event Event, format CallbackFormat) ([]byte, error) {
	if format == "" || format == CallbackFormatLegacy {
		return json.Marshal(DecisionRequestFromEvent(event))
	}
	if format != CallbackFormatOctoCardV1 {
		return nil, errors.New("cardactiondispatch: unsupported callback format")
	}
	if event.EventID <= 0 || event.Card.TemplateID == "" || event.Card.TemplateVersion == "" || event.Card.View == "" {
		return nil, errors.New("cardactiondispatch: octo-card callback context is incomplete")
	}
	inputs := event.Inputs
	if inputs == nil {
		inputs = map[string]interface{}{}
	}
	return json.Marshal(callbackEnvelope{
		Protocol: "octo-card@1.0",
		Type:     "card.action",
		Action: callbackAction{
			ID: event.ActionID, Data: event.Data, Inputs: inputs,
		},
		Card: callbackCard{
			TemplateID: event.Card.TemplateID, TemplateVersion: event.Card.TemplateVersion, View: event.Card.View,
			MessageID: event.MessageID, ChannelID: event.ChannelID, ChannelType: event.ChannelType,
		},
		Actor:     callbackActor{UID: event.OperatorUID, SpaceID: event.OperatorSpaceID},
		TriggerID: fmt.Sprintf("%d", event.EventID),
	})
}

func marshalDecisionRequest(request DecisionRequest, format CallbackFormat) ([]byte, error) {
	if format == "" || format == CallbackFormatLegacy {
		return json.Marshal(request)
	}
	if request.event == nil {
		return nil, errors.New("cardactiondispatch: callback request has no source event")
	}
	// Routes may opt into the new format while pre-registry cards are still in
	// flight. A completely absent CardContext identifies that legacy path and
	// preserves its flat payload; partial context still reaches
	// MarshalCallbackRequest and fails closed as corrupt/incomplete metadata.
	if format == CallbackFormatOctoCardV1 && request.event.Card == (CardContext{}) {
		return json.Marshal(request)
	}
	return MarshalCallbackRequest(*request.event, format)
}

func DecodeDecisionResult(reader io.Reader) (DecisionResult, error) {
	if reader == nil {
		return DecisionResult{}, errors.New("cardactiondispatch: empty decision response")
	}
	limited := io.LimitReader(reader, MaxDecisionResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("cardactiondispatch: read decision response: %w", err)
	}
	if len(body) > MaxDecisionResponseBytes {
		return DecisionResult{}, errors.New("cardactiondispatch: decision response too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result DecisionResult
	if err := decoder.Decode(&result); err != nil {
		return DecisionResult{}, fmt.Errorf("cardactiondispatch: decode decision response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DecisionResult{}, err
	}
	if !validDisposition(result.Disposition) || !validState(result.State) {
		return DecisionResult{}, errors.New("cardactiondispatch: invalid decision result enum")
	}
	if len(result.RequesterUID) > 128 || strings.TrimSpace(result.RequesterUID) != result.RequesterUID {
		return DecisionResult{}, errors.New("cardactiondispatch: invalid requester_uid")
	}
	if (result.State == StateApproved || result.State == StateDenied) && result.RequesterUID == "" {
		return DecisionResult{}, errors.New("cardactiondispatch: terminal decision requires requester_uid")
	}
	// Authoritative decider identity is optional (legacy callbacks omit it) but,
	// when present, is typed and bounded: it feeds an internal user/Space lookup,
	// never raw display. decider_space_id without decider_uid is meaningless.
	if err := ValidateAuthoritativeDeciderIDs(result.DeciderUID, result.DeciderSpaceID); err != nil {
		return DecisionResult{}, err
	}
	if result.DecidedAt < 0 {
		return DecisionResult{}, errors.New("cardactiondispatch: invalid decided_at")
	}
	if len(result.Display) > 32 {
		return DecisionResult{}, errors.New("cardactiondispatch: too many display fields")
	}
	for key, value := range result.Display {
		if key == "" || len(key) > 64 || utf8.RuneCountInString(value) > 500 {
			return DecisionResult{}, errors.New("cardactiondispatch: invalid display field")
		}
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("cardactiondispatch: trailing decision response")
		}
		return fmt.Errorf("cardactiondispatch: decode trailing decision response: %w", err)
	}
	return nil
}

func validDisposition(value Disposition) bool {
	switch value {
	case DispositionApplied, DispositionReplayed, DispositionForbidden, DispositionConflict, DispositionNotFound:
		return true
	default:
		return false
	}
}

func validState(value State) bool {
	switch value {
	case StatePending, StateApproved, StateDenied, StateCancelled:
		return true
	default:
		return false
	}
}

func stringField(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}
