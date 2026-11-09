// Package permissionbundle implements the server's two-stage storage contract.
// It deliberately contains no invocation or capability decision evaluator.
package permissionbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"unicode/utf8"
)

var (
	ErrInvalid     = errors.New("permission: invalid bundle")
	ErrVersion     = errors.New("permission: unsupported version")
	ErrConflict    = errors.New("permission: revision conflict")
	ErrForbidden   = errors.New("permission: scope forbidden")
	ErrReference   = errors.New("permission: reference unavailable")
	ErrUnavailable = errors.New("permission: bundle unavailable")
)

const MaxBundleBytes = 262144

// decodeStrict rejects information that encoding/json would otherwise discard
// or replace: decoded duplicate keys, invalid UTF-8 and unpaired UTF-16 escapes.
func decodeStrict(r io.Reader, limit int) (any, error) {
	return decodeStrictDepth(r, limit, 12)
}

func decodeStrictDepth(r io.Reader, limit, maxDepth int) (any, error) {
	return decodeBudget(r, limit, maxDepth, nil)
}

// budget is shared by all v2 metadata files. It counts values before allocation;
// nil retains the original policy/v1 depth semantics.
func decodeBudget(r io.Reader, limit, maxDepth int, budget *int) (any, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil || len(b) > limit || !utf8.Valid(b) || !pairedEscapes(b) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := valueCount(d, 0, maxDepth, budget)
	if err != nil {
		return nil, ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	return v, nil
}

func pairedEscapes(b []byte) bool {
	inside := false
	for i := 0; i < len(b); i++ {
		if b[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || b[i] != '\\' {
			continue
		}
		i++
		if i >= len(b) {
			return false
		}
		if b[i] != 'u' {
			continue
		}
		if i+4 >= len(b) {
			return false
		}
		n, e := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inside
}

func value(d *json.Decoder, depth, maxDepth int) (any, error) {
	return valueCount(d, depth, maxDepth, nil)
}

func valueCount(d *json.Decoder, depth, maxDepth int, budget *int) (any, error) {
	if budget != nil {
		if *budget <= 0 || depth >= maxDepth {
			return nil, ErrInvalid
		}
		*budget--
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		if depth >= maxDepth {
			return nil, ErrInvalid
		}
		m := map[string]any{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, ErrInvalid
			}
			if _, ok := m[key]; ok {
				return nil, ErrInvalid
			}
			v, err := valueCount(d, depth+1, maxDepth, budget)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return m, nil
	case json.Delim('['):
		if depth >= maxDepth {
			return nil, ErrInvalid
		}
		a := []any{}
		for d.More() {
			v, err := valueCount(d, depth+1, maxDepth, budget)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return a, nil
	default:
		if _, ok := t.(json.Delim); ok {
			return nil, ErrInvalid
		}
		return t, nil
	}
}

// canonical implements permission-canonical-json-v1, including literal UTF-8
// U+2028/U+2029 and HTML characters (encoding/json's default is different).
func canonical(v any) []byte {
	var b bytes.Buffer
	var emit func(any)
	emit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				quote(&b, k)
				b.WriteByte(':')
				emit(x[k])
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, v := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				emit(v)
			}
			b.WriteByte(']')
		case string:
			quote(&b, x)
		case json.Number:
			b.WriteString(string(x))
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case nil:
			b.WriteString("null")
		default:
			panic("permission canonical: unvalidated type")
		}
	}
	emit(v)
	return b.Bytes()
}

func quote(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 32 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&15])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
