package permissionbundle

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type valueSpan struct{ start, end int }
type renderedWork struct {
	hash                string
	bytes, nodes, depth int
}

// JSON has already passed its declared renderer's syntax/depth validation. This scanner
// records original value spans so substitution never parse/remarshals raw roots
// or reorders members/numeric spellings. Only the declared renderer formats the
// substituted RawMessage afterward.
func jsonValueSpans(raw []byte) (map[string]valueSpan, map[string]int) {
	spans := map[string]valueSpan{}
	counts := map[string]int{}
	i := 0
	skip := func() {
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\n' || raw[i] == '\r' || raw[i] == '\t') {
			i++
		}
	}
	str := func() {
		i++
		for i < len(raw) {
			if raw[i] == '\\' {
				i += 2
				continue
			}
			if raw[i] == '"' {
				i++
				break
			}
			i++
		}
	}
	var scan func(string)
	scan = func(path string) {
		skip()
		start := i
		switch raw[i] {
		case '{':
			i++
			skip()
			for raw[i] != '}' {
				startKey := i
				str()
				var key string
				_ = json.Unmarshal(raw[startKey:i], &key)
				skip()
				i++
				scan(path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"))
				skip()
				if raw[i] == ',' {
					i++
					skip()
				} else {
					break
				}
			}
			i++
		case '[':
			i++
			skip()
			n := 0
			for raw[i] != ']' {
				scan(path + "/" + strconv.Itoa(n))
				n++
				skip()
				if raw[i] == ',' {
					i++
					skip()
				} else {
					break
				}
			}
			i++
		case '"':
			str()
		default:
			for i < len(raw) && !strings.ContainsRune(" \r\n\t,]}", rune(raw[i])) {
				i++
			}
		}
		spans[path] = valueSpan{start, i}
		counts[path]++
	}
	scan("")
	return spans, counts
}

func templateSpans(raw []byte, c map[string]any) (map[string]valueSpan, error) {
	value, _, _, e := rawJSONWork(raw, 8388608, 32, 1048576, true)
	if e != nil {
		return nil, packageError("template JSON")
	}
	spans, counts := jsonValueSpans(raw)
	declared := map[string]string{}
	ids := map[string]bool{}
	for _, value := range c["slots"].([]any) {
		slot := value.(map[string]any)
		id, path := slot["id"].(string), slot["pointer"].(string)
		if ids[id] || declared[path] != "" {
			return nil, packageError("duplicate slot")
		}
		ids[id] = true
		declared[path] = id
		for at := path; ; {
			if counts[at] != 1 {
				return nil, packageError("ambiguous slot pointer")
			}
			if at == "" {
				break
			}
			at = at[:strings.LastIndex(at, "/")]
		}
	}
	found := map[string]valueSpan{}
	var visit func(any, string) error
	visit = func(value any, path string) error {
		switch x := value.(type) {
		case rawObject:
			for _, member := range x {
				if member.key == "$octo_slot" {
					id, declaredHere := declared[path]
					if !declaredHere || len(x) != 1 || id != member.value || found[path].end != 0 {
						return packageError("undeclared slot marker")
					}
					found[path] = spans[path]
					return nil
				}
			}
			for _, member := range x {
				if e := visit(member.value, path+"/"+strings.ReplaceAll(strings.ReplaceAll(member.key, "~", "~0"), "/", "~1")); e != nil {
					return e
				}
			}
		case []any:
			for i, v := range x {
				if e := visit(v, path+"/"+strconv.Itoa(i)); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if e := visit(value, ""); e != nil {
		return nil, e
	}
	if len(found) != len(declared) {
		return nil, packageError("missing slot marker")
	}
	return found, nil
}

func slotReplacement(domain string, value any) ([]byte, error) {
	if domain == "server_managed_argument_names" {
		switch names := value.(type) {
		case []string:
			for _, name := range names {
				if !utf8.ValidString(name) {
					return nil, packageError("managed names scalar")
				}
			}
		case []any:
			for _, name := range names {
				s, ok := name.(string)
				if !ok || !utf8.ValidString(s) {
					return nil, packageError("managed names scalar")
				}
			}
		default:
			return nil, packageError("managed names type")
		}
	}
	raw, e := json.Marshal(value)
	if e != nil {
		return nil, packageError("slot value")
	}
	remaining := 1048576
	x, e := decodeBudget(bytes.NewReader(raw), 65536, 32, &remaining)
	if e != nil {
		return nil, packageError("slot bounds")
	}
	switch domain {
	case "facade_hint":
		s, ok := x.(string)
		original, originalOK := value.(string)
		if !ok || !originalOK || !utf8.ValidString(original) || s != original {
			return nil, packageError("facade slot type")
		}
	case "server_managed_argument_names":
		names, ok := x.([]any)
		if !ok {
			return nil, packageError("managed names type")
		}
		last := ""
		for _, name := range names {
			s, ok := name.(string)
			if !ok || s == "" || s <= last {
				return nil, packageError("managed names order")
			}
			last = s
		}
	default:
		return nil, packageError("slot domain")
	}
	return raw, nil
}

func renderPackageTemplate(raw []byte, c map[string]any, spans map[string]valueSpan, values map[string]any) ([]byte, error) {
	type replacement struct {
		valueSpan
		raw []byte
	}
	replacements := []replacement{}
	for _, value := range c["slots"].([]any) {
		slot := value.(map[string]any)
		domain := slot["domain"].(string)
		v, ok := values[domain]
		if !ok {
			return nil, packageError("missing source slot assignment")
		}
		b, e := slotReplacement(domain, v)
		if e != nil {
			return nil, e
		}
		max, e := inheritedInt(slot["max_length"], 65536)
		if e != nil || len(b) > max {
			return nil, packageError("slot max_length")
		}
		replacements = append(replacements, replacement{spans[slot["pointer"].(string)], b})
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	size := len(raw)
	for _, r := range replacements {
		size -= r.end - r.start
		if addWork(&size, len(r.raw), 16777216) != nil {
			return nil, packageError("slot expansion")
		}
	}
	var out bytes.Buffer
	out.Grow(size)
	at := 0
	for _, r := range replacements {
		if r.start < at {
			return nil, packageError("overlapping slots")
		}
		out.Write(raw[at:r.start])
		out.Write(r.raw)
		at = r.end
	}
	out.Write(raw[at:])
	// RawMessage preserves source member order and number spelling; the
	// inherited fixed renderer alone applies indentation and HTML escaping.
	if renderedSize(out.Bytes()) > 16777216 {
		return nil, packageError("rendered byte bound")
	}
	b, e := json.MarshalIndent(json.RawMessage(out.Bytes()), "", "  ")
	if e != nil || len(b) > 16777216 {
		return nil, packageError("slot renderer")
	}
	return b, nil
}

// Exact size of Go's RawMessage + two-space MarshalIndent for validated JSON.
// Count before allowing the renderer to allocate its formatted output.
func renderedSize(raw []byte) int {
	n, depth := 0, 0
	nonempty := []bool{}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case ' ', '\r', '\n', '\t':
			continue
		case '"':
			n++
			for i++; i < len(raw); i++ {
				c = raw[i]
				if c == '"' {
					n++
					break
				}
				if c == '\\' {
					n += 2
					i++
					continue
				}
				if c == '<' || c == '>' || c == '&' {
					n += 6
					continue
				}
				if c == 0xe2 && i+2 < len(raw) && raw[i+1] == 0x80 && (raw[i+2] == 0xa8 || raw[i+2] == 0xa9) {
					n += 6
					i += 2
					continue
				}
				n++
			}
		case '{', '[':
			n++
			depth++
			next := i + 1
			for next < len(raw) && strings.ContainsRune(" \r\n\t", rune(raw[next])) {
				next++
			}
			has := next < len(raw) && raw[next] != '}' && raw[next] != ']'
			nonempty = append(nonempty, has)
			if has {
				n += 1 + 2*depth
			}
		case '}', ']':
			depth--
			if nonempty[len(nonempty)-1] {
				n += 1 + 2*depth
			}
			nonempty = nonempty[:len(nonempty)-1]
			n++
		case ',':
			n += 2 + 2*depth
		case ':':
			n += 2
		default:
			n++
		}
	}
	return n
}

func (p *packageCheck) postSlots() error {
	v := p.v
	v.ledger.PostNodes = v.ledger.MetadataNodes
	charge := func(b, n int) error {
		if addWork(&v.ledger.PostBytes, b, 16777216) != nil || addWork(&v.ledger.PostNodes, n, 1048576) != nil {
			return packageError("post accounting")
		}
		return nil
	}
	for id, c := range v.content {
		if p.ctx.Err() != nil {
			return packageError("deadline")
		}
		switch c["kind"] {
		case "bytes":
			ref := c["data_ref"].(string)
			if e := charge(len(v.roots[ref]), p.rootNodes[ref]); e != nil {
				return e
			}
		case "discovery_index":
			for _, row := range c["rows"].([]any) {
				ref := v.rows[row.(string)]["row_ref"].(string)
				if e := charge(len(v.roots[ref]), p.rootNodes[ref]); e != nil {
					return e
				}
			}
		case "template":
			ref := c["value_ref"].(string)
			raw := v.roots[ref]
			spans, e := templateSpans(raw, c)
			if e != nil {
				return e
			}
			cells := p.templateCells[id]
			if len(cells) == 0 {
				if len(spans) != 0 {
					return packageError("slot template has no source cells")
				}
				cells = []SlotCell{{Pin: v.pin}}
			}
			cache := map[string]renderedWork{}
			maxBytes, maxNodes := 0, 0
			for _, cell := range cells {
				if p.ctx.Err() != nil {
					return packageError("deadline")
				}
				values := map[string]any{}
				if len(spans) != 0 {
					if p.slots == nil {
						return packageError("source joint slot domain unavailable")
					}
					values, e = p.slots(p.ctx, cell)
					if e != nil {
						return packageError("source joint slot domain")
					}
					for domain, value := range values {
						if domain != "facade_hint" && domain != "server_managed_argument_names" {
							return packageError("unknown source slot domain")
						}
						// Validate every joint assignment before a render-cache hit.
						// Different Go types can marshal to the same JSON bytes.
						if _, e := slotReplacement(domain, value); e != nil {
							return e
						}
					}
				}
				keyBytes, e := json.Marshal(values)
				if e != nil || len(keyBytes) > 131072 {
					return packageError("joint slot assignment")
				}
				key := hash(keyBytes)
				work, ok := cache[key]
				if !ok {
					b, e := renderPackageTemplate(raw, c, spans, values)
					if e != nil {
						return e
					}
					_, n, depth, e := rawJSONWork(b, 16777216, 32, 1048576, false)
					if e != nil {
						return packageError("post JSON/depth/nodes")
					}
					work = renderedWork{hash(b), len(b), n, depth}
					cache[key] = work
				}
				if work.bytes > maxBytes {
					maxBytes = work.bytes
				}
				if work.nodes > maxNodes {
					maxNodes = work.nodes
				}
				if work.depth > v.ledger.MaxDepth {
					v.ledger.MaxDepth = work.depth
				}
				if cell.EntryID != "" {
					r := 0
					if cell.Resources {
						r = 1
					}
					f := 0
					for i, s := range serverFacades {
						if cell.ServerFacade == s {
							f = i
						}
					}
					v.rendered[cellKey(cell.EntryID, cell.Facade, r, cell.ManagedMask, f)] = work.hash
				}
			}
			if e := charge(maxBytes, maxNodes); e != nil {
				return e
			}
		}
	}
	for id, raw := range v.roots {
		if p.uses[id] == 0 {
			if e := charge(len(raw), p.rootNodes[id]); e != nil {
				return e
			}
		}
	}
	return nil
}
