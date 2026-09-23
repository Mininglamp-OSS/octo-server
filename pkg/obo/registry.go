package obo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Local routes derive their Action from a trusted handler, never from the Bot.
// Resolve callers submit an Action, which must be present in ActionRegistry.
var localActions = map[string]map[string]struct{}{
	"project.read":        {"ALL": {}},
	"project.member.read": {"ALL": {}},
}

type ActionRegistry struct {
	actions map[string]map[string]struct{}
}

// ParseActionRegistry reads OCTO_OBO_ACTION_SCOPES_JSON. Built-in server
// Actions are always available; additional downstream Actions are explicitly
// registered at startup. This is policy configuration, not caller identity.
func ParseActionRegistry(raw string) (*ActionRegistry, error) {
	r := &ActionRegistry{actions: make(map[string]map[string]struct{}, len(localActions))}
	for action, scopes := range localActions {
		copied := make(map[string]struct{}, len(scopes))
		for scope := range scopes {
			copied[scope] = struct{}{}
		}
		r.actions[action] = copied
	}
	if strings.TrimSpace(raw) == "" {
		return r, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	open, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("obo: invalid Action registry: %w", err)
	}
	if open != json.Delim('{') {
		return nil, errors.New("obo: Action registry must be an object")
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("obo: invalid Action key: %w", err)
		}
		action, ok := key.(string)
		if !ok || action == "" || strings.TrimSpace(action) != action {
			return nil, errors.New("obo: invalid Action key")
		}
		if _, duplicate := r.actions[action]; duplicate {
			return nil, fmt.Errorf("obo: duplicate Action %q", action)
		}
		var scopes []string
		if err := decoder.Decode(&scopes); err != nil {
			return nil, fmt.Errorf("obo: Action %q scopes: %w", action, err)
		}
		if len(scopes) != 1 || scopes[0] != "ALL" {
			return nil, fmt.Errorf("obo: Action %q must explicitly allow ALL Scope", action)
		}
		r.actions[action] = map[string]struct{}{"ALL": {}}
	}
	if closeToken, err := decoder.Token(); err != nil || closeToken != json.Delim('}') {
		return nil, errors.New("obo: invalid Action registry object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("obo: trailing Action registry data")
	}
	return r, nil
}

func (r *ActionRegistry) Allows(action string) bool {
	if r == nil {
		return false
	}
	_, allowed := r.actions[action]
	return allowed
}

func (r *ActionRegistry) AllowsScope(action, scope string) bool {
	if r == nil {
		return false
	}
	_, allowed := r.actions[action][scope]
	return allowed
}

// MatchALL is the first-scope policy for local server routes. Adding an
// external Action never silently expands these routes' capability.
func MatchALL(action string, bindings []string) bool {
	if _, allowed := localActions[action]["ALL"]; !allowed {
		return false
	}
	for _, binding := range bindings {
		if binding == "ALL" {
			return true
		}
	}
	return false
}
