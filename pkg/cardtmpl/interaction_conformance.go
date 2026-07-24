package cardtmpl

import (
	"fmt"
	"sort"
)

// assertInteractionReport locks the machine-readable v2 interaction report to
// one rendered sample. Reports are consumed by capability discovery and SDKs,
// so publishing an action or input that the card does not actually contain is
// a startup-fatal contract error rather than a runtime compatibility surprise.
func assertInteractionReport(payload map[string]any, report InteractionReport) error {
	card, _ := payload["card"].(map[string]any)
	if card == nil {
		return fmt.Errorf("interaction conformance: card missing")
	}

	actualActions := make(map[string]map[string]any)
	actualInputs := make(map[string]map[string]any)
	walkInteractionNodes(card["body"], actualActions, actualInputs)
	walkInteractionNodes(card["actions"], actualActions, actualInputs)
	walkInteractionNodes(card["selectAction"], actualActions, actualInputs)

	declaredActions := make(map[string]DeclaredAction)
	for _, action := range report.Actions {
		if action.Type == "Action.Submit" {
			declaredActions[action.ID] = action
		}
	}
	declaredInputs := make(map[string]DeclaredInput)
	for _, input := range report.Inputs {
		declaredInputs[input.ID] = input
	}

	if got, want := sortedMapKeys(actualActions), sortedMapKeys(declaredActions); !equalStrings(got, want) {
		return fmt.Errorf("interaction conformance: Action.Submit ids got=%v want=%v", got, want)
	}
	if got, want := sortedMapKeys(actualInputs), sortedMapKeys(declaredInputs); !equalStrings(got, want) {
		return fmt.Errorf("interaction conformance: Input ids got=%v want=%v", got, want)
	}

	for id, declared := range declaredActions {
		action := actualActions[id]
		data, _ := action["data"].(map[string]any)
		gotKeys := sortedMapKeys(data)
		wantKeys := append([]string(nil), declared.DataKeys...)
		sort.Strings(wantKeys)
		if !equalStrings(gotKeys, wantKeys) {
			return fmt.Errorf("interaction conformance: Action.Submit %q data keys got=%v want=%v", id, gotKeys, wantKeys)
		}
		if declared.AssociatedInputs != "" {
			got, _ := action["associatedInputs"].(string)
			if got == "" {
				got = "auto"
			}
			if got != declared.AssociatedInputs {
				return fmt.Errorf("interaction conformance: Action.Submit %q associatedInputs=%q want=%q",
					id, got, declared.AssociatedInputs)
			}
		}
	}
	return nil
}

func walkInteractionNodes(value any, actions, inputs map[string]map[string]any) {
	switch node := value.(type) {
	case []any:
		for _, child := range node {
			walkInteractionNodes(child, actions, inputs)
		}
	case map[string]any:
		typeName, _ := node["type"].(string)
		if typeName == "Action.Submit" {
			if id, _ := node["id"].(string); id != "" {
				actions[id] = node
			}
		}
		if len(typeName) > len("Input.") && typeName[:len("Input.")] == "Input." {
			if id, _ := node["id"].(string); id != "" {
				inputs[id] = node
			}
		}
		for _, key := range []string{"body", "actions", "items", "columns", "rows", "cells", "selectAction"} {
			if child, ok := node[key]; ok {
				walkInteractionNodes(child, actions, inputs)
			}
		}
	}
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
