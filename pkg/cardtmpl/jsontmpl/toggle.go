package jsontmpl

import (
	"fmt"
	"sort"
	"strings"
)

// ValidateToggleTargets statically checks that every Action.ToggleVisibility
// target in the template refers to an element id that exists in the same
// template. It walks the raw (pre-expansion) AST: element ids and toggle
// targets in these cards are template-authored constants, so a dangling target
// is an authoring bug catchable at register time rather than a silent no-op
// button at render time. Data-driven ids/targets (`${...}`) are skipped — they
// cannot be resolved statically.
func ValidateToggleTargets(node any) error {
	ids := map[string]struct{}{}
	collectElementIDs(node, ids)

	var missing []string
	seen := map[string]struct{}{}
	collectToggleTargets(node, func(target string) {
		if strings.Contains(target, "${") {
			return // data-driven target; not statically verifiable
		}
		if _, ok := ids[target]; ok {
			return
		}
		if _, dup := seen[target]; dup {
			return
		}
		seen[target] = struct{}{}
		missing = append(missing, target)
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("jsontmpl: Action.ToggleVisibility targets not found in template: %v", missing)
	}
	return nil
}

func collectElementIDs(node any, out map[string]struct{}) {
	switch n := node.(type) {
	case map[string]any:
		if id, ok := n["id"].(string); ok && id != "" && !strings.Contains(id, "${") {
			out[id] = struct{}{}
		}
		for _, v := range n {
			collectElementIDs(v, out)
		}
	case []any:
		for _, v := range n {
			collectElementIDs(v, out)
		}
	}
}

func collectToggleTargets(node any, emit func(string)) {
	switch n := node.(type) {
	case map[string]any:
		if t, _ := n["type"].(string); t == "Action.ToggleVisibility" {
			if targets, ok := n["targetElements"].([]any); ok {
				for _, tv := range targets {
					switch tt := tv.(type) {
					case string:
						emit(tt)
					case map[string]any:
						// AC also allows {elementId, isVisible} target objects.
						if id, ok := tt["elementId"].(string); ok {
							emit(id)
						}
					}
				}
			}
		}
		for _, v := range n {
			collectToggleTargets(v, emit)
		}
	case []any:
		for _, v := range n {
			collectToggleTargets(v, emit)
		}
	}
}
