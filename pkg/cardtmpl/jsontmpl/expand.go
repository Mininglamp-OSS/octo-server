package jsontmpl

import (
	"context"
	"fmt"
)

// dataDirective is the reserved key that marks an element for $data-driven
// repetition; it is consumed during expansion and never emitted.
const dataDirective = "$data"

// maxExpandNodes bounds the total number of nodes a single Expand may emit. It is
// a fail-closed backstop against unbounded / multiplicative $data expansion
// (CWE-400/770, PR #654 review): a caller-supplied $data array repeats its element
// template once per item and nested $data multiplies, so without a ceiling a large
// array would allocate the whole tree before cardmsg.Validate — the functional
// MaxNodes gate, which renderCore only runs AFTER Build — ever sees it. The cap
// sits far above the largest shipped golden (~600 nodes) so it never rejects a
// legitimate template, but turns a runaway array from unbounded allocation into a
// fast, bounded error.
const maxExpandNodes = 10000

// Expand walks a parsed Adaptive Card template (map/slice/scalar tree) and
// returns a new tree with every ${...} expression resolved against sc. Objects
// carrying a `$data` directive are repeated once per array element (with a loop
// scope exposing `$index`); the directive key itself is dropped. String leaves
// go through EvalValue, so a whole-value `"${bool}"` becomes a native boolean
// and caller data is escaped. The input tree is never mutated.
//
// ctx is honored between nodes so a caller cancel/deadline interrupts a large
// expansion (R2-7 context propagation); the total emitted nodes are bounded by
// maxExpandNodes (see the const). A nil ctx is treated as context.Background().
func Expand(ctx context.Context, node any, sc Scope, escape EscapeFunc) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ex := &expander{ctx: ctx, escape: escape, budget: maxExpandNodes}
	return ex.expand(node, sc)
}

// expander carries the per-Expand state shared across the whole recursion: the
// caller ctx (for cancellation), the data escaper, and the remaining node budget.
type expander struct {
	ctx    context.Context
	escape EscapeFunc
	budget int
}

// expand dispatches on node kind, decrementing the shared node budget and
// checking the caller ctx before each node so a runaway/cancelled expansion stops
// promptly instead of allocating the full tree.
func (e *expander) expand(node any, sc Scope) (any, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	if e.budget <= 0 {
		return nil, fmt.Errorf("jsontmpl: expansion exceeded %d-node budget (possible unbounded $data)", maxExpandNodes)
	}
	e.budget--
	switch n := node.(type) {
	case map[string]any:
		return e.expandObject(n, sc)
	case []any:
		return e.expandArray(n, sc)
	case string:
		return EvalValue(n, sc, e.escape)
	default:
		// bool / float64 / nil literals pass through unchanged.
		return node, nil
	}
}

// expandObject expands every value of m under sc, skipping the $data directive
// key (repetition is handled by expandArray on the parent).
func (e *expander) expandObject(m map[string]any, sc Scope) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == dataDirective {
			continue
		}
		ev, err := e.expand(v, sc)
		if err != nil {
			return nil, err
		}
		out[k] = ev
	}
	return out, nil
}

// expandArray expands each element; an element object carrying `$data` is
// repeated once per resolved array item under a loop scope. Each repetition is
// budget-counted (via expandObject → expand on its values) and ctx-checked, so a
// large or multiplicative $data array fails fast rather than allocating in full.
func (e *expander) expandArray(arr []any, sc Scope) ([]any, error) {
	out := make([]any, 0, len(arr))
	for _, el := range arr {
		if m, ok := el.(map[string]any); ok {
			if raw, has := m[dataDirective]; has {
				items, err := e.resolveDataArray(raw, sc)
				if err != nil {
					return nil, err
				}
				for i, item := range items {
					if err := e.ctx.Err(); err != nil {
						return nil, err
					}
					// Charge one budget unit per repetition BEFORE expanding it.
					// expandObject only decrements for the element's *value* fields,
					// so a directive-only element ({"$data":"${arr}"} with no sibling
					// keys) would otherwise charge zero per item and let a large caller
					// array bypass the maxExpandNodes ceiling entirely (PR #654 review:
					// Jerry-Xin / yujiawei P1 — the emitted repetition is itself a node).
					if e.budget <= 0 {
						return nil, fmt.Errorf("jsontmpl: expansion exceeded %d-node budget (possible unbounded $data)", maxExpandNodes)
					}
					e.budget--
					itemData, ok := item.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("jsontmpl: $data element %d is %T, want object", i, item)
					}
					expanded, err := e.expandObject(m, Scope{Data: itemData, Index: i, InLoop: true})
					if err != nil {
						return nil, err
					}
					out = append(out, expanded)
				}
				continue
			}
		}
		expanded, err := e.expand(el, sc)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// resolveDataArray evaluates a `$data` binding (a `"${field}"` string) to the
// backing array. The result must be a JSON array; anything else is an error.
func (e *expander) resolveDataArray(raw any, sc Scope) ([]any, error) {
	expr, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("jsontmpl: $data must be a %q string, got %T", "${...}", raw)
	}
	v, err := EvalValue(expr, sc, e.escape)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("jsontmpl: $data %q resolved to %T, want array", expr, v)
	}
	return arr, nil
}
