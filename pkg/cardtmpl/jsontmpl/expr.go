// Package jsontmpl is a dependency-free evaluator for the bounded subset of the
// Adaptive Card Templating language used by Registry-backed JSON card templates
// (roadmap E1). It intentionally implements only the syntax exercised by the
// shipped handoff templates — single-identifier field bindings, `$index`,
// `if($index == N, a, b)`, string interpolation, and typed whole-value binding.
// Anything outside that subset is a hard error, never a silent passthrough
// (see the package brief, decision D4: freeze the subset, extend via L0 PR).
//
// The package holds no cardtmpl dependency: the markdown escaper is injected as
// an EscapeFunc so the trust boundary (template text is trusted, caller-supplied
// data is escaped) is enforced by the caller that owns the escaping authority.
package jsontmpl

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Scope is the data frame an expression is evaluated against. Data is the
// current object (root object, or the current element inside a $data loop);
// Index/InLoop carry the iteration position for `$index`.
type Scope struct {
	Data   map[string]any
	Index  int
	InLoop bool
}

// EscapeFunc escapes a caller-supplied string before it lands in card text.
// The expander injects cardtmpl's markdown escaper here.
type EscapeFunc func(string) string

// EvalValue evaluates a JSON string value that may embed ${...} expressions:
//   - no ${...}                     → the literal string, unchanged
//   - exactly one ${expr} (no other text) → the typed result of expr; a bool or
//     number stays native, a string result is escaped
//   - literal text mixed with ${expr}(s)  → string interpolation: each expr
//     result is stringified and escaped, literals are emitted verbatim (so
//     trusted template punctuation is never mangled, only data is escaped)
func EvalValue(raw string, sc Scope, escape EscapeFunc) (any, error) {
	segs, err := splitSegments(raw)
	if err != nil {
		return nil, err
	}
	if len(segs) == 1 && !segs[0].isExpr {
		return segs[0].text, nil
	}
	if len(segs) == 1 && segs[0].isExpr {
		v, err := evalExpr(segs[0].text, sc)
		if err != nil {
			return nil, err
		}
		if s, ok := v.(string); ok {
			return escape(s), nil
		}
		return v, nil
	}
	var b strings.Builder
	for _, seg := range segs {
		if !seg.isExpr {
			b.WriteString(seg.text)
			continue
		}
		v, err := evalExpr(seg.text, sc)
		if err != nil {
			return nil, err
		}
		b.WriteString(escape(stringify(v)))
	}
	return b.String(), nil
}

type segment struct {
	text   string
	isExpr bool
}

// splitSegments splits raw into alternating literal / ${expr} segments. No
// nested braces occur in the supported subset, so the first '}' after '${'
// closes the expression.
func splitSegments(raw string) ([]segment, error) {
	var segs []segment
	for {
		i := strings.Index(raw, "${")
		if i < 0 {
			if raw != "" {
				segs = append(segs, segment{text: raw})
			}
			break
		}
		if i > 0 {
			segs = append(segs, segment{text: raw[:i]})
		}
		rest := raw[i+2:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			return nil, fmt.Errorf("jsontmpl: unterminated ${ in %q", raw)
		}
		segs = append(segs, segment{text: strings.TrimSpace(rest[:j]), isExpr: true})
		raw = rest[j+1:]
	}
	if len(segs) == 0 {
		segs = append(segs, segment{text: ""})
	}
	return segs, nil
}

// evalExpr evaluates the inside of a ${...}: an if(...) call, `$index`, an
// integer/bool/'string' literal, or a single identifier resolved in scope.
func evalExpr(src string, sc Scope) (any, error) {
	src = strings.TrimSpace(src)
	if strings.HasPrefix(src, "if(") && strings.HasSuffix(src, ")") {
		return evalIf(src[len("if("):len(src)-1], sc)
	}
	return evalOperand(src, sc)
}

// evalIf evaluates the comma-separated args of if(cond, then, else).
func evalIf(args string, sc Scope) (any, error) {
	parts, err := splitArgs(args)
	if err != nil {
		return nil, err
	}
	if len(parts) != 3 {
		return nil, fmt.Errorf("jsontmpl: if() takes 3 args, got %d in %q", len(parts), args)
	}
	cond, err := evalCond(parts[0], sc)
	if err != nil {
		return nil, err
	}
	if cond {
		return evalOperand(strings.TrimSpace(parts[1]), sc)
	}
	return evalOperand(strings.TrimSpace(parts[2]), sc)
}

// evalCond evaluates a `LEFT == RIGHT` equality condition (the only comparison
// in the supported subset).
func evalCond(src string, sc Scope) (bool, error) {
	src = strings.TrimSpace(src)
	lraw, rraw, ok := splitEquality(src)
	if !ok {
		return false, fmt.Errorf("jsontmpl: unsupported condition %q (only == is supported)", src)
	}
	left, err := evalOperand(strings.TrimSpace(lraw), sc)
	if err != nil {
		return false, err
	}
	right, err := evalOperand(strings.TrimSpace(rraw), sc)
	if err != nil {
		return false, err
	}
	return equalOperands(left, right)
}

// splitEquality finds the top-level `==` operator that is not inside a single-
// quoted string literal and returns the two operand substrings. ok=false when
// there is no such operator (so `'a==b'` alone is not mistaken for a comparison).
func splitEquality(s string) (left, right string, ok bool) {
	inQuote := false
	for i := 0; i+1 < len(s); i++ {
		switch s[i] {
		case '\'':
			inQuote = !inQuote
		case '=':
			if !inQuote && s[i+1] == '=' {
				return s[:i], s[i+2:], true
			}
		}
	}
	return "", "", false
}

// equalOperands compares two evaluated operands with equality semantics for the
// bounded subset (string / bool / number). Numbers coerce across int and float64
// — JSON numbers unmarshal to float64 while integer literals and $index are int,
// and Go's `==` is false across differing concrete types, so a raw `left == right`
// would take the wrong branch for `${if(count == 1, …)}` on JSON data. Kinds that
// `==` cannot compare (slice / map / func) return a clean error instead of
// panicking. (PR #654 review.)
func equalOperands(l, r any) (bool, error) {
	if lf, lok := toFloat(l); lok {
		rf, rok := toFloat(r)
		return rok && lf == rf, nil // number vs non-number is never equal
	}
	switch l.(type) {
	case string, bool, nil:
		// r's dynamic type may differ (e.g. string vs bool); comparing two
		// interfaces of differing comparable types is false, not a panic.
		switch r.(type) {
		case string, bool, nil:
			return l == r, nil
		default:
			return false, nil
		}
	default:
		return false, fmt.Errorf("jsontmpl: cannot compare %T with == (unsupported operand type)", l)
	}
}

// toFloat coerces the numeric kinds the evaluator produces to float64 for
// cross-type equality; ok=false for non-numeric values.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		parsed, err := strconv.ParseFloat(n.String(), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// evalOperand resolves a single token: `$index`, an integer/bool/'string'
// literal, or an identifier looked up in scope data.
func evalOperand(tok string, sc Scope) (any, error) {
	tok = strings.TrimSpace(tok)
	switch {
	case tok == "$index":
		if !sc.InLoop {
			return nil, fmt.Errorf("jsontmpl: $index used outside a $data loop")
		}
		return sc.Index, nil
	case tok == "true":
		return true, nil
	case tok == "false":
		return false, nil
	case len(tok) >= 2 && tok[0] == '\'' && tok[len(tok)-1] == '\'':
		return tok[1 : len(tok)-1], nil
	}
	if n, err := strconv.Atoi(tok); err == nil {
		return n, nil
	}
	if !isIdentifier(tok) {
		return nil, fmt.Errorf("jsontmpl: unsupported expression token %q", tok)
	}
	v, ok := sc.Data[tok]
	if !ok {
		return nil, fmt.Errorf("jsontmpl: field %q not found in data", tok)
	}
	return v, nil
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// splitArgs splits comma-separated args, respecting single-quoted literals.
func splitArgs(s string) ([]string, error) {
	var (
		parts   []string
		cur     strings.Builder
		inQuote bool
	)
	for _, r := range s {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("jsontmpl: unterminated quote in %q", s)
	}
	parts = append(parts, cur.String())
	return parts, nil
}

// stringify renders a non-string eval result for interpolation.
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case json.Number:
		parsed, err := strconv.ParseFloat(t.String(), 64)
		if err == nil {
			return strconv.FormatFloat(parsed, 'g', -1, 64)
		}
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}
