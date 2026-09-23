package obo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type Mode string

const (
	ModeAsBot Mode = "AS_BOT"
	ModeOBO   Mode = "OBO"
)

type Request struct {
	BotToken string `json:"bot_token"`
	Mode     Mode   `json:"mode"`
	SpaceID  string `json:"space_id"`
	Action   string `json:"action,omitempty"`
	Resource *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"resource,omitempty"`
}

type Identity struct {
	UID     string `json:"uid"`
	Kind    string `json:"kind"`
	SpaceID string `json:"space_id"`
}

type Delegation struct {
	GrantID       int64  `json:"grant_id"`
	MatchedScope  string `json:"matched_scope"`
	PolicyVersion int64  `json:"policy_version"`
	Action        string `json:"action"`
	DecisionID    string `json:"decision_id"`
}

type Principal struct {
	Mode       Mode        `json:"mode"`
	Actor      Identity    `json:"actor"`
	Subject    Identity    `json:"subject"`
	Delegation *Delegation `json:"delegation,omitempty"`
}

// Snapshot contains only authorization facts read from one consistent DB
// snapshot. A read-only Bot request does not populate delegation fields.
type Snapshot struct {
	BotUID        string
	OwnerUID      string
	GrantID       int64
	PolicyVersion int64
	BoundScopes   []string
}

type SnapshotReader interface {
	Read(ctx context.Context, botToken, spaceID string, mode Mode) (Snapshot, error)
}

type DecisionError struct {
	Code   string
	Status int
	Cause  error
}

func (e *DecisionError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("obo: %s: %v", e.Code, e.Cause)
	}
	return "obo: " + e.Code
}

func (e *DecisionError) Unwrap() error { return e.Cause }

func deny(code string, status int) error { return &DecisionError{Code: code, Status: status} }

func infrastructure(err error) error {
	return &DecisionError{Code: "infra_failure", Status: http.StatusServiceUnavailable, Cause: err}
}

func DecisionCode(err error) string {
	var decision *DecisionError
	if errors.As(err, &decision) {
		return decision.Code
	}
	return "infra_failure"
}

type Resolver struct {
	Reader   SnapshotReader
	Registry *ActionRegistry
}

func (r *Resolver) Resolve(ctx context.Context, req Request) (*Principal, error) {
	if req.Mode != ModeAsBot && req.Mode != ModeOBO {
		return nil, deny("invalid_mode", http.StatusBadRequest)
	}
	if strings.TrimSpace(req.SpaceID) == "" {
		return nil, deny("missing_space_id", http.StatusBadRequest)
	}
	if req.BotToken == "" || !strings.HasPrefix(req.BotToken, "bf_") {
		return nil, deny("invalid_credential", http.StatusUnauthorized)
	}
	if req.Mode == ModeAsBot {
		if req.Action != "" || req.Resource != nil {
			return nil, deny("invalid_request", http.StatusBadRequest)
		}
	} else {
		if req.Action == "" {
			return nil, deny("invalid_request", http.StatusBadRequest)
		}
		if !r.Registry.Allows(req.Action) {
			return nil, deny("action_not_allowed", http.StatusForbidden)
		}
	}
	if r.Reader == nil {
		return nil, infrastructure(errors.New("snapshot reader is not configured"))
	}
	state, err := r.Reader.Read(ctx, req.BotToken, req.SpaceID, req.Mode)
	if err != nil {
		var decision *DecisionError
		if errors.As(err, &decision) {
			return nil, decision
		}
		return nil, infrastructure(err)
	}
	if state.BotUID == "" {
		return nil, infrastructure(errors.New("snapshot reader returned no Bot UID"))
	}
	actor := Identity{UID: state.BotUID, Kind: "BOT", SpaceID: req.SpaceID}
	if req.Mode == ModeAsBot {
		return &Principal{Mode: ModeAsBot, Actor: actor, Subject: actor}, nil
	}
	if state.OwnerUID == "" || state.GrantID <= 0 {
		return nil, deny("delegation_denied", http.StatusForbidden)
	}
	matched := false
	for _, scope := range state.BoundScopes {
		if r.Registry.AllowsScope(req.Action, scope) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, deny("delegation_denied", http.StatusForbidden)
	}
	var decisionID [16]byte
	if _, err := rand.Read(decisionID[:]); err != nil {
		return nil, infrastructure(err)
	}
	return &Principal{
		Mode:    ModeOBO,
		Actor:   actor,
		Subject: Identity{UID: state.OwnerUID, Kind: "HUMAN", SpaceID: req.SpaceID},
		Delegation: &Delegation{
			GrantID: state.GrantID, MatchedScope: "ALL", PolicyVersion: state.PolicyVersion,
			Action: req.Action, DecisionID: hex.EncodeToString(decisionID[:]),
		},
	}, nil
}
