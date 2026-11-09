package permissionbundle

import (
	"bytes"
	"encoding/json"
	"io"
)

type rawMember struct {
	key   string
	value any
}
type rawObject []rawMember

// rawJSONWork applies the declared Go JSON renderer's syntax domain and counts
// every value, including duplicate object members. It deliberately does NOT use
// metadata canonical/UTF-8/surrogate/number filters. The exact raw bytes remain
// the committed/rendered value. keep is needed only to inspect template markers.
func rawJSONWork(raw []byte, limit, depthLimit, nodeLimit int, keep bool) (any, int, int, error) {
	if len(raw) > limit {
		return nil, 0, 0, ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes, maxDepth := 0, 0
	var value func(int) (any, error)
	value = func(depth int) (any, error) {
		if depth > depthLimit || nodes >= nodeLimit {
			return nil, ErrUnavailable
		}
		nodes++
		if depth > maxDepth {
			maxDepth = depth
		}
		t, e := d.Token()
		if e != nil {
			return nil, e
		}
		switch t {
		case json.Delim('{'):
			var members rawObject
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return nil, e
				}
				name, ok := key.(string)
				if !ok {
					return nil, ErrUnavailable
				}
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				if keep {
					members = append(members, rawMember{name, v})
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return nil, ErrUnavailable
			}
			return members, nil
		case json.Delim('['):
			var items []any
			for d.More() {
				v, e := value(depth + 1)
				if e != nil {
					return nil, e
				}
				if keep {
					items = append(items, v)
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return nil, ErrUnavailable
			}
			return items, nil
		default:
			if _, ok := t.(json.Delim); ok {
				return nil, ErrUnavailable
			}
			return t, nil
		}
	}
	v, e := value(1)
	if e != nil {
		return nil, 0, 0, e
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, 0, 0, ErrUnavailable
	}
	return v, nodes, maxDepth, nil
}
