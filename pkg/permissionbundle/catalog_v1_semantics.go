package permissionbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
)

// normalizeArtifactV1 builds an internal semantic view, never an emitted v2
// package, new supported pin or response fallback. Legacy serving remains on its
// original validator. Ambiguous old selector/static maps cannot approve a pair.
func normalizeArtifactV1(ctx context.Context, release Release, slots SlotValues) (*VerifiedCatalog, error) {
	loadPackageSchemas()
	if packageSchemas.err != nil {
		return nil, ErrUnavailable
	}
	x, e := decodeStrictDepth(bytes.NewReader(release.Bytes), 8388608, 32)
	if e != nil {
		return nil, ErrUnavailable
	}
	old := x.(map[string]any)
	v := &VerifiedCatalog{pin: release.Pin, files: map[string][]byte{}, entries: map[string]map[string]any{}, content: map[string]map[string]any{}, roots: map[string][]byte{}, rows: map[string]map[string]any{}, profiles: map[string]map[string]any{}, selected: map[string]string{}, rendered: map[string]string{}, manifest: old}
	p := &packageCheck{v: v, ctx: ctx, budget: 1048576, owners: map[string]string{}, rootRecords: map[string]map[string]any{}, jsonRoots: map[string]bool{}, rootNodes: map[string]int{}, uses: map[string]int{}, templateCells: map[string][]SlotCell{}, renderedSlots: map[string]string{}, slots: slots, unpartitioned: true}
	root := func(raw []byte) string {
		id := hash(raw)
		v.roots[id] = raw
		p.rootRecords[id] = map[string]any{"sha256": id, "length": json.Number(strconv.Itoa(len(raw))), "parts": []any{}}
		return id
	}
	remap := map[string]string{}
	oldContent := map[string]map[string]any{}
	put := func(c map[string]any) string {
		id := descriptorID(c)
		if c["kind"] != "schema_variant" {
			c["sha256"] = id
		}
		v.content[id] = c
		return id
	}
	for _, value := range old["content"].([]any) {
		c := value.(map[string]any)
		oldContent[descriptorID(c)] = c
	}
	for id, c := range oldContent {
		if c["kind"] != "bytes" && c["kind"] != "template" {
			continue
		}
		d := objectWithout(c, "data_base64", "value_base64")
		if c["kind"] == "bytes" {
			raw, e := catalogBlob(c["data_base64"].(string))
			if e != nil {
				return nil, e
			}
			d["data_ref"] = root(raw)
		} else {
			raw, e := catalogBlob(c["value_base64"].(string))
			if e != nil {
				return nil, e
			}
			d["value_ref"] = root(raw)
		}
		remap[id] = put(d)
	}
	for id, c := range oldContent {
		d := objectWithout(c, "sha256")
		switch c["kind"] {
		case "schema_variant":
			template, truth := remap[c["template_hash"].(string)], remap[c["parameter_truth_hash"].(string)]
			if template == "" || truth == "" {
				return nil, ErrUnavailable
			}
			d["template_hash"] = template
			d["parameter_truth_hash"] = truth
			d["selector"] = map[string]any{"resources_bits": json.Number("3"), "managed_bits": json.Number("255"), "facade_bits": json.Number("7")}
			remap[id] = put(d)
		case "discovery_index":
			refs := []any{}
			for _, value := range c["rows"].([]any) {
				row := value.(map[string]any)
				raw, e := catalogBlob(row["row_base64"].(string))
				if e != nil {
					return nil, e
				}
				r := map[string]any{"operation_id": row["operation_id"], "match_text": row["match_text"], "row_ref": root(raw)}
				h := hash(canonical(r))
				r["sha256"] = h
				v.rows[h] = r
				refs = append(refs, h)
			}
			d["rows"] = refs
			d["selector"] = map[string]any{"resources_bits": json.Number("3"), "facade_bits": json.Number("7")}
			remap[id] = put(d)
		}
	}
	for _, value := range old["entries"].([]any) {
		e := value.(map[string]any)
		id := e["id"].(string)
		if !sameIdentity(e, packageSchemas.identities[id]) {
			return nil, packageError("old identity semantics")
		}
		refs := []any{}
		for _, value := range e["content_refs"].([]any) {
			mapped := remap[value.(string)]
			if mapped == "" {
				return nil, ErrUnavailable
			}
			refs = append(refs, mapped)
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].(string) < refs[j].(string) })
		e["content_refs"] = refs
		v.entries[id] = e
	}
	for _, value := range old["profiles"].([]any) {
		profile := value.(map[string]any)
		v.profiles[profile["name"].(string)] = profile
		if e := validatePackageProfile(profile, v.entries); e != nil {
			return nil, e
		}
	}
	bindings := []any{}
	for _, id := range sortedObjectKeys(v.entries) {
		e := v.entries[id]
		if e["kind"] != "resource" && e["kind"] != "inventory" && id != "octo-cli/discovery/module-map" {
			continue
		}
		possible := []string{}
		for _, ref := range e["content_refs"].([]any) {
			c := v.content[ref.(string)]
			if c["kind"] == "bytes" || c["kind"] == "template" {
				possible = append(possible, ref.(string))
			}
		}
		if len(possible) != 1 {
			return nil, packageError("old static map ambiguous")
		}
		bindings = append(bindings, map[string]any{"entry_id": id, "content_ref": possible[0], "selector": map[string]any{"resources_bits": json.Number("3"), "managed_bits": json.Number("255"), "facade_bits": json.Number("7")}})
	}
	v.index = map[string]any{"profiles": old["profiles"], "source_files": old["source_files"], "exceptions": old["exceptions"], "static_bindings": bindings}
	if e := p.referencesAndUses(); e != nil {
		return nil, e
	}
	// Recheck exact declared raw domains and finite comparison work. This is
	// not a v2 stored/expansion ledger for the legacy artifact.
	for id, raw := range v.roots {
		weight := p.uses[id]
		if weight == 0 {
			weight = 1
		}
		if len(raw) > 16777216/weight || addWork(&v.ledger.PreBytes, len(raw)*weight, 16777216) != nil {
			return nil, ErrUnavailable
		}
		if p.jsonRoots[id] {
			remaining := (1048576 - v.ledger.PreNodes) / weight
			_, nodes, _, err := rawJSONWork(raw, 8388608, 32, remaining, false)
			if err != nil {
				return nil, ErrUnavailable
			}
			p.rootNodes[id] = nodes
			v.ledger.PreNodes += p.rootNodes[id] * weight
		}
	}
	if e := p.closure(); e != nil {
		return nil, e
	}
	if e := p.selectors(); e != nil {
		return nil, e
	}
	if e := p.postSlots(); e != nil {
		return nil, e
	}
	v.semantic = p.semanticView()
	if ctx.Err() != nil || v.semantic == nil {
		return nil, ErrUnavailable
	}
	return v, nil
}
