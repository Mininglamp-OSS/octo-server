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
	EventID     int64                  `json:"event_id"`
	SenderUID   string                 `json:"sender_uid"`
	Owner       string                 `json:"owner"`
	ActionType  string                 `json:"action_type"`
	MessageID   string                 `json:"message_id"`
	ChannelID   string                 `json:"channel_id"`
	ChannelType uint8                  `json:"channel_type"`
	SpaceID     string                 `json:"space_id,omitempty"`
	ActionID    string                 `json:"action_id"`
	OperatorUID string                 `json:"operator_uid"`
	ClientToken string                 `json:"client_token,omitempty"`
	ActedAt     int64                  `json:"acted_at"`
	Inputs      map[string]interface{} `json:"inputs"`
	Data        map[string]interface{} `json:"data,omitempty"`
}

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
	ActedAt     int64                  `json:"acted_at"`
}

type DecisionResult struct {
	Disposition  Disposition       `json:"disposition"`
	State        State             `json:"state"`
	RequesterUID string            `json:"requester_uid,omitempty"`
	Display      map[string]string `json:"display,omitempty"`
}

func DecisionRequestFromEvent(event Event) DecisionRequest {
	inputs := event.Inputs
	if inputs == nil {
		inputs = map[string]interface{}{}
	}
	return DecisionRequest{
		EventID:     event.EventID,
		ActionID:    event.ActionID,
		Decision:    stringField(event.Data, "decision"),
		OperatorUID: event.OperatorUID,
		DocID:       stringField(event.Data, "doc_id"),
		RequestID:   stringField(event.Data, "request_id"),
		Inputs:      inputs,
		Data:        event.Data,
		MessageID:   event.MessageID,
		ChannelID:   event.ChannelID,
		ChannelType: event.ChannelType,
		SpaceID:     event.SpaceID,
		ActedAt:     event.ActedAt,
	}
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
