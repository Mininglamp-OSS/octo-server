package jsontmpl

import "fmt"

// dataDirective is the reserved key that marks an element for $data-driven
// repetition; it is consumed during expansion and never emitted.
const dataDirective = "$data"

// Expand walks a parsed Adaptive Card template (map/slice/scalar tree) and
// returns a new tree with every ${...} expression resolved against sc. Objects
// carrying a `$data` directive are repeated once per array element (with a loop
// scope exposing `$index`); the directive key itself is dropped. String leaves
// go through EvalValue, so a whole-value `"${bool}"` becomes a native boolean
// and caller data is escaped. The input tree is never mutated.
func Expand(node any, sc Scope, escape EscapeFunc) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		return expandObject(n, sc, escape)
	case []any:
		return expandArray(n, sc, escape)
	case string:
		return EvalValue(n, sc, escape)
	default:
		// bool / float64 / nil literals pass through unchanged.
		return node, nil
	}
}

// expandObject expands every value of m under sc, skipping the $data directive
// key (repetition is handled by expandArray on the parent).
func expandObject(m map[string]any, sc Scope, escape EscapeFunc) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == dataDirective {
			continue
		}
		ev, err := Expand(v, sc, escape)
		if err != nil {
			return nil, err
		}
		out[k] = ev
	}
	return out, nil
}

// expandArray expands each element; an element object carrying `$data` is
// repeated once per resolved array item under a loop scope.
func expandArray(arr []any, sc Scope, escape EscapeFunc) ([]any, error) {
	out := make([]any, 0, len(arr))
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			if raw, has := m[dataDirective]; has {
				items, err := resolveDataArray(raw, sc, escape)
				if err != nil {
					return nil, err
				}
				for i, item := range items {
					itemData, ok := item.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("jsontmpl: $data element %d is %T, want object", i, item)
					}
					expanded, err := expandObject(m, Scope{Data: itemData, Index: i, InLoop: true}, escape)
					if err != nil {
						return nil, err
					}
					out = append(out, expanded)
				}
				continue
			}
		}
		expanded, err := Expand(el, sc, escape)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// resolveDataArray evaluates a `$data` binding (a `"${field}"` string) to the
// backing array. The result must be a JSON array; anything else is an error.
func resolveDataArray(raw any, sc Scope, escape EscapeFunc) ([]any, error) {
	expr, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("jsontmpl: $data must be a %q string, got %T", "${...}", raw)
	}
	v, err := EvalValue(expr, sc, escape)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("jsontmpl: $data %q resolved to %T, want array", expr, v)
	}
	return arr, nil
}
