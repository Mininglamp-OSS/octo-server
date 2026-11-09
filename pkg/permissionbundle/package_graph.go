package permissionbundle

import (
	"bytes"
	"sort"
)

func entryDependencies(e map[string]any, profiles map[string]map[string]any) ([]string, error) {
	p := profiles[e["profile"].(string)]
	if p == nil {
		return nil, packageError("entry profile")
	}
	deps := map[string]bool{}
	for _, x := range p["children"].([]any) {
		deps[x.(string)] = true
	}
	for _, phase := range []string{"prepare", "dispatch", "publish"} {
		for _, x := range p[phase].([]any) {
			a := x.(map[string]any)
			if a["capability"] != "self" {
				deps[a["capability"].(string)] = true
			}
			b := a["target_template"].(map[string]any)["binding"].(map[string]any)
			if b["kind"] == "capability" {
				deps[b["id"].(string)] = true
			}
		}
	}
	delete(deps, e["id"].(string)) // self is the bound root, not recursive expansion
	result := make([]string, 0, len(deps))
	for id := range deps {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func (p *packageCheck) closure() error {
	v := p.v
	deps := map[string][]string{}
	for id, e := range v.entries {
		var err error
		deps[id], err = entryDependencies(e, v.profiles)
		if err != nil {
			return err
		}
		last := ""
		for _, x := range e["content_refs"].([]any) {
			ref := x.(string)
			if ref <= last || v.content[ref] == nil {
				return packageError("entry content edge")
			}
			last = ref
		}
	}
	state := map[string]int{}
	var acyclic func(string) bool
	acyclic = func(id string) bool {
		if v.entries[id] == nil || state[id] == 1 {
			return false
		}
		if state[id] == 2 {
			return true
		}
		state[id] = 1
		for _, next := range deps[id] {
			if !acyclic(next) {
				return false
			}
		}
		state[id] = 2
		return true
	}
	for id := range v.entries {
		if !acyclic(id) {
			return packageError("entry dependency cycle/type")
		}
	}
	contentLabels := map[string]map[string]bool{}
	seenEntries := map[string]bool{}
	seenRoots := map[string]bool{}
	seenRows := map[string]bool{}
	var visitContent func(string, string) error
	visitContent = func(id, label string) error {
		c := v.content[id]
		if c == nil {
			return packageError("content edge")
		}
		if contentLabels[id] == nil {
			contentLabels[id] = map[string]bool{}
		}
		if contentLabels[id][label] {
			return nil
		}
		contentLabels[id][label] = true
		switch c["kind"] {
		case "bytes":
			seenRoots[c["data_ref"].(string)] = true
		case "template":
			seenRoots[c["value_ref"].(string)] = true
		case "schema_variant":
			for _, field := range []string{"template_hash", "parameter_truth_hash"} {
				if err := visitContent(c[field].(string), label); err != nil {
					return err
				}
			}
		case "discovery_index":
			for _, value := range c["rows"].([]any) {
				id := value.(string)
				r := v.rows[id]
				if r == nil {
					return packageError("typed row")
				}
				seenRows[id] = true
				seenRoots[r["row_ref"].(string)] = true
			}
		}
		return nil
	}
	var visitEntry func(string, string) error
	visitEntry = func(id, label string) error {
		if seenEntries[label+"/"+id] {
			return nil
		}
		seenEntries[label+"/"+id] = true
		for _, value := range v.entries[id]["content_refs"].([]any) {
			if err := visitContent(value.(string), label); err != nil {
				return err
			}
		}
		for _, next := range deps[id] {
			if err := visitEntry(next, label); err != nil {
				return err
			}
		}
		return nil
	}
	for id, e := range v.entries {
		if err := visitEntry(id, entryModule(e)); err != nil {
			return err
		}
	}
	last := ""
	for _, value := range v.index["source_files"].([]any) {
		tuple := value.([]any)
		path, ref := tuple[0].(string), tuple[1].(string)
		if path == "" || path <= last || v.content[ref]["kind"] != "bytes" {
			return packageError("source root/order")
		}
		last = path
		if err := visitContent(ref, "_global"); err != nil {
			return err
		}
	}
	for id := range v.content {
		lowest := ""
		for label := range contentLabels[id] {
			if lowest == "" || label < lowest {
				lowest = label
			}
		}
		if p.unpartitioned {
			p.owners[id] = lowest
		}
		if lowest == "" || lowest != p.owners[id] {
			return packageError("transitive content owner/orphan")
		}
	}
	if len(seenRoots) != len(v.roots) || len(seenRows) != len(v.rows) {
		return packageError("orphan root/row")
	}
	intervals := map[string][][2]int{}
	for id := range seenRoots {
		for _, value := range p.rootRecords[id]["parts"].([]any) {
			part := value.(map[string]any)
			offset, _ := packagingInt(part["offset"], 1048576)
			n, _ := packagingInt(part["length"], 1048576)
			path := part["path"].(string)
			intervals[path] = append(intervals[path], [2]int{offset, offset + n})
		}
	}
	for path, raw := range v.files {
		kind, _, _, _ := packagePath(path)
		if kind != "blob" {
			continue
		}
		spans := intervals[path]
		sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
		end := 0
		for _, span := range spans {
			if span[0] > end {
				return packageError("hidden blob padding")
			}
			if span[1] > end {
				end = span[1]
			}
		}
		if end != len(raw) {
			return packageError("unused blob range")
		}
	}
	return nil
}

func selectorTuples(s map[string]any, schema bool) ([][3]int, error) {
	r, e := packagingInt(s["resources_bits"], 3)
	f, e2 := packagingInt(s["facade_bits"], 7)
	m := 1
	if schema {
		var err error
		m, err = packagingInt(s["managed_bits"], 255)
		if err != nil {
			return nil, packageError("managed selector")
		}
	}
	if e != nil || e2 != nil || r == 0 || f == 0 || m == 0 {
		return nil, packageError("selector bits")
	}
	var out [][3]int
	for ri := 0; ri < 2; ri++ {
		for mi := 0; mi < 8; mi++ {
			for fi := 0; fi < 3; fi++ {
				if r&(1<<ri) != 0 && m&(1<<mi) != 0 && f&(1<<fi) != 0 {
					out = append(out, [3]int{ri, mi, fi})
				}
			}
		}
	}
	return out, nil
}
func (p *packageCheck) addSelection(entry, facade, descriptor string, selector map[string]any, schema bool) error {
	tuples, e := selectorTuples(selector, schema)
	if e != nil {
		return e
	}
	for _, t := range tuples {
		key := cellKey(entry, facade, t[0], t[1], t[2])
		if p.v.selected[key] != "" {
			return packageError("selector overlap")
		}
		p.v.selected[key] = descriptor
		c := p.v.content[descriptor]
		if c["kind"] == "schema_variant" {
			c = p.v.content[c["template_hash"].(string)]
		}
		if c["kind"] == "template" {
			id := descriptorID(c)
			p.templateCells[id] = append(p.templateCells[id], SlotCell{p.v.pin, entry, facade, t[0] == 1, t[1], serverFacades[t[2]]})
		}
	}
	return nil
}

func hasContent(e map[string]any, id string) bool {
	for _, r := range e["content_refs"].([]any) {
		if r == id {
			return true
		}
	}
	return false
}

func (p *packageCheck) selectors() error {
	v := p.v
	schemaEntries := map[string]string{}
	operations := map[string]bool{}
	operationServices := map[string]string{}
	discoveryEntries := map[string]bool{}
	staticEntries := map[string]bool{}
	for id, e := range v.entries {
		switch e["kind"] {
		case "operation":
			operations[e["target"].([]any)[1].(string)] = true
			operationServices[e["target"].([]any)[1].(string)] = e["target"].([]any)[0].(string)
		case "schema":
			schemaEntries[e["target"].([]any)[1].(string)] = id
		case "resource", "inventory":
			staticEntries[id] = true
		case "discovery":
			if id == "octo-cli/discovery/module-map" {
				staticEntries[id] = true
			} else {
				discoveryEntries[id] = true
			}
		}
	}
	for id, c := range v.content {
		switch c["kind"] {
		case "schema_variant":
			op := c["operation_id"].(string)
			entry := schemaEntries[op]
			if entry == "" || !hasContent(v.entries[entry], id) {
				return packageError("schema entry binding")
			}
			if e := p.addSelection(entry, c["facade"].(string), id, c["selector"].(map[string]any), true); e != nil {
				return e
			}
		case "discovery_index":
			scope := c["scope"].(map[string]any)
			entry := "octo-cli/discovery/search-all"
			if scope["kind"] == "named" {
				entry = "octo-cli/" + scope["service"].(string) + "/discovery/search"
			}
			if !discoveryEntries[entry] || !hasContent(v.entries[entry], id) {
				return packageError("discovery entry binding")
			}
			seen := map[string]bool{}
			for _, row := range c["rows"].([]any) {
				op := v.rows[row.(string)]["operation_id"].(string)
				if !operations[op] || seen[op] || scope["kind"] == "named" && operationServices[op] != scope["service"] {
					return packageError("discovery operation")
				}
				seen[op] = true
			}
			for op, service := range operationServices {
				if (scope["kind"] == "global" || scope["service"] == service) && !seen[op] {
					return packageError("discovery inventory gap")
				}
			}
			if e := p.addSelection(entry, c["facade"].(string), id, c["selector"].(map[string]any), false); e != nil {
				return e
			}
		}
	}
	previous := ""
	var lastTuple [3]int
	for _, value := range v.index["static_bindings"].([]any) {
		b := value.(map[string]any)
		entry, ref := b["entry_id"].(string), b["content_ref"].(string)
		selector := b["selector"].(map[string]any)
		tuple := [3]int{}
		for i, k := range []string{"resources_bits", "managed_bits", "facade_bits"} {
			tuple[i], _ = packagingInt(selector[k], 255)
		}
		if !staticEntries[entry] || !hasContent(v.entries[entry], ref) || v.content[ref]["kind"] != "bytes" && v.content[ref]["kind"] != "template" || entry < previous || entry == previous && !tupleLess(lastTuple, tuple) {
			return packageError("static binding/order")
		}
		previous = entry
		lastTuple = tuple
		if e := p.addSelection(entry, "static", ref, selector, true); e != nil {
			return e
		}
	}
	for _, entry := range schemaEntries {
		for _, facade := range []string{"describe_op", "get_skill_full", "get_skill_summary"} {
			for r := 0; r < 2; r++ {
				for m := 0; m < 8; m++ {
					for f := 0; f < 3; f++ {
						if v.selected[cellKey(entry, facade, r, m, f)] == "" {
							return packageError("schema selector gap")
						}
						v.ledger.SchemaCells++
					}
				}
			}
		}
	}
	for entry := range discoveryEntries {
		for _, facade := range []string{"search_ops", "get_skill_search"} {
			for r := 0; r < 2; r++ {
				for f := 0; f < 3; f++ {
					if v.selected[cellKey(entry, facade, r, 0, f)] == "" {
						return packageError("discovery selector gap")
					}
					v.ledger.DiscoveryCells++
				}
			}
		}
	}
	for entry := range staticEntries {
		for r := 0; r < 2; r++ {
			for m := 0; m < 8; m++ {
				for f := 0; f < 3; f++ {
					if v.selected[cellKey(entry, "static", r, m, f)] == "" {
						return packageError("static selector gap")
					}
					v.ledger.StaticCells++
				}
			}
		}
	}
	if v.ledger.SchemaCells != 56592 || v.ledger.DiscoveryCells != 156 || v.ledger.StaticCells != 1248 {
		return packageError("selector census")
	}
	return nil
}
func tupleLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// The normalized view erases only physical packaging. It retains raw hashes,
// complete slots/renderer/profile/source bindings and every finite selection.
func (p *packageCheck) semanticView() []byte {
	v := p.v
	descriptors := map[string]any{}
	normalized := map[string]any{}
	refs := map[string]string{}
	collision := false
	var normalize func(string) any
	normalize = func(id string) any {
		if value, ok := normalized[id]; ok {
			return value
		}
		c := v.content[id]
		d := objectWithout(c, "sha256", "selector")
		switch c["kind"] {
		case "bytes":
			delete(d, "data_ref")
			d["raw_sha256"] = c["data_ref"]
		case "template":
			delete(d, "value_ref")
			d["raw_sha256"] = c["value_ref"]
		case "schema_variant":
			delete(d, "template_hash")
			delete(d, "parameter_truth_hash")
			d["template"] = normalize(c["template_hash"].(string))
			d["parameter_truth"] = normalize(c["parameter_truth_hash"].(string))
		case "discovery_index":
			rows := []any{}
			for _, value := range c["rows"].([]any) {
				row := v.rows[value.(string)]
				rows = append(rows, map[string]any{"operation_id": row["operation_id"], "match_text": row["match_text"], "raw_sha256": row["row_ref"]})
			}
			d["rows"] = rows
		}
		normalized[id] = d
		return d
	}
	semanticRef := func(id string) string {
		if ref := refs[id]; ref != "" {
			return ref
		}
		value := normalize(id)
		key := hash(canonical(value))
		if existing, ok := descriptors[key]; ok && !bytes.Equal(canonical(existing), canonical(value)) {
			collision = true
		}
		descriptors[key] = value
		refs[id] = key
		return key
	}
	entries := []any{}
	for _, id := range sortedObjectKeys(v.entries) {
		e := objectWithout(v.entries[id], "content_refs")
		unique := map[string]any{}
		for _, r := range v.entries[id]["content_refs"].([]any) {
			ref := semanticRef(r.(string))
			unique[ref] = ref
		}
		keys := make([]string, 0, len(unique))
		for k := range unique {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		entryRefs := []any{}
		for _, k := range keys {
			entryRefs = append(entryRefs, unique[k])
		}
		e["content"] = entryRefs
		entries = append(entries, e)
	}
	selection := map[string]any{}
	for id := range v.content {
		semanticRef(id)
	}
	for key, id := range v.selected {
		selection[key] = semanticRef(id)
	}
	rendered := map[string]any{}
	for key, h := range v.rendered {
		rendered[key] = h
	}
	d := map[string]any{"entries": entries, "selection": selection, "descriptors": descriptors, "rendered": rendered, "profiles": v.index["profiles"], "source_files": v.index["source_files"], "exceptions": v.index["exceptions"]}
	for _, k := range []string{"semantics_version", "identity_index_sha256", "source_heads", "normalization"} {
		d[k] = v.manifest[k]
	}
	limits := objectWithout(v.manifest["limits"].(map[string]any), "packaging")
	d["limits"] = limits
	if collision {
		return nil
	}
	return canonical(d)
}

func semanticEqual(a, b *VerifiedCatalog) bool {
	return hash(a.semantic) == hash(b.semantic) && bytes.Equal(a.semantic, b.semantic)
}
