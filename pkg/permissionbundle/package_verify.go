package permissionbundle

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
)

type packageCheck struct {
	v             *VerifiedCatalog
	ctx           context.Context
	budget        int
	owners        map[string]string
	rootRecords   map[string]map[string]any
	jsonRoots     map[string]bool
	rootNodes     map[string]int
	uses          map[string]int
	templateCells map[string][]SlotCell
	renderedSlots map[string]string
	slots         SlotValues
	unpartitioned bool // private v1 semantic normalization only, never v2 admission
}

func packageError(step string) error {
	return fmt.Errorf("%w: catalog package %s", ErrUnavailable, step)
}

// VerifyCatalogPackage independently verifies a complete private byte snapshot.
// The caller separately owns support/source conformance; a valid hash or this
// function's success is never an allowlist entry or an authorization decision.
func VerifyCatalogPackage(ctx context.Context, input PackageInput, slots SlotValues) (*VerifiedCatalog, error) {
	loadPackageSchemas()
	if packageSchemas.err != nil || ctx.Err() != nil || len(input.Files) < 2 || len(input.Files) > 64 {
		return nil, packageError("inventory")
	}
	v := &VerifiedCatalog{pin: input.Pin, files: map[string][]byte{}, entries: map[string]map[string]any{}, content: map[string]map[string]any{}, roots: map[string][]byte{}, rows: map[string]map[string]any{}, profiles: map[string]map[string]any{}, selected: map[string]string{}, rendered: map[string]string{}}
	p := &packageCheck{v: v, ctx: ctx, budget: 1048576, owners: map[string]string{}, rootRecords: map[string]map[string]any{}, jsonRoots: map[string]bool{}, rootNodes: map[string]int{}, uses: map[string]int{}, templateCells: map[string][]SlotCell{}, renderedSlots: map[string]string{}, slots: slots}
	for path, raw := range input.Files {
		kind, _, _, ok := packagePath(path)
		limit := 262144
		if path == "manifest.json" {
			limit = 65536
			ok = true
		} else if kind == "blob" {
			limit = 1048576
		}
		if !ok || len(raw) == 0 || len(raw) > limit || addWork(&v.ledger.StoredBytes, len(raw), 8388608) != nil || ctx.Err() != nil {
			return nil, packageError("file bounds")
		}
		v.files[path] = bytes.Clone(raw)
	}
	raw := v.files["manifest.json"]
	manifest, e := p.metadata("manifest", raw)
	if e != nil || manifest["catalog_version"] != input.Pin.Version || hash(raw) != input.Pin.Digest {
		return nil, packageError("manifest pin/shape")
	}
	if n, e := packagingInt(manifest["contract_revision"], 3); e != nil || n != 3 {
		return nil, packageError("revision")
	}
	// Constant numeric spellings in new packaging fields are exact. This does
	// not recursively filter inherited profiles, ScalarLiteral or raw JSON.
	var schema map[string]any
	if e := decodeInto(packageSchemaBytes, &schema); e != nil {
		return nil, e
	}
	limitConstant := schema["$defs"].(map[string]any)["Manifest"].(map[string]any)["properties"].(map[string]any)["limits"].(map[string]any)["const"]
	if !bytes.Equal(canonical(manifest["limits"]), canonical(limitConstant)) {
		return nil, packageError("limit constants")
	}
	v.manifest = manifest
	listed := map[string]bool{"manifest.json": true}
	shards := map[string][]int{}
	previous := ""
	for _, value := range manifest["files"].([]any) {
		f := value.(map[string]any)
		path := f["path"].(string)
		kind, module, shard, ok := packagePath(path)
		limit := 262144
		if kind == "blob" {
			limit = 1048576
		}
		n, e := packagingInt(f["bytes"], limit)
		if !ok || path <= previous || listed[path] || f["kind"] != kind || e != nil || n == 0 || len(v.files[path]) != n || hash(v.files[path]) != f["sha256"] {
			return nil, packageError("listed file")
		}
		previous = path
		listed[path] = true
		if kind != "index" {
			shards[kind+"/"+module] = append(shards[kind+"/"+module], shard)
		}
		if kind == "blob" {
			continue
		}
		m, e := p.metadata(kind, v.files[path])
		if e != nil {
			return nil, e
		}
		switch kind {
		case "index":
			v.index = m
		case "module":
			if m["module"] != module || !numberEquals(m["shard"], shard) || len(m["entries"].([]any))+len(m["content"].([]any)) == 0 {
				return nil, packageError("module partition")
			}
			last := ""
			for _, value := range m["entries"].([]any) {
				e := value.(map[string]any)
				id := e["id"].(string)
				if id <= last || v.entries[id] != nil || !sameIdentity(e, packageSchemas.identities[id]) || entryModule(e) != module {
					return nil, packageError("entry identity/owner")
				}
				v.entries[id] = e
				last = id
			}
			last = ""
			for _, value := range m["content"].([]any) {
				c := value.(map[string]any)
				id := descriptorID(c)
				if id <= last || v.content[id] != nil || c["kind"] != "schema_variant" && c["sha256"] != id {
					return nil, packageError("content hash/order")
				}
				v.content[id] = c
				p.owners[id] = module
				last = id
			}
		case "shared":
			if !numberEquals(m["shard"], shard) || len(m["roots"].([]any))+len(m["rows"].([]any)) == 0 {
				return nil, packageError("shared partition")
			}
			last := ""
			for _, value := range m["roots"].([]any) {
				r := value.(map[string]any)
				id := r["sha256"].(string)
				if id <= last || p.rootRecords[id] != nil {
					return nil, packageError("root order")
				}
				p.rootRecords[id] = r
				last = id
			}
			last = ""
			for _, value := range m["rows"].([]any) {
				r := value.(map[string]any)
				id := r["sha256"].(string)
				if id <= last || v.rows[id] != nil || hash(canonical(objectWithout(r, "sha256"))) != id {
					return nil, packageError("row hash/order")
				}
				v.rows[id] = r
				last = id
			}
		}
	}
	if len(listed) != len(v.files) || v.index == nil || len(v.entries) != 827 || len(v.content) > 16384 || len(p.rootRecords) > 8192 || len(v.rows) > 4096 {
		return nil, packageError("complete inventory")
	}
	for _, m := range packageModules {
		if len(shards["module/"+m]) == 0 {
			return nil, packageError("missing module")
		}
	}
	for _, ns := range shards {
		sort.Ints(ns)
		for i, n := range ns {
			if n != i {
				return nil, packageError("shard gap")
			}
		}
	}
	previous = ""
	for _, value := range v.index["profiles"].([]any) {
		profile := value.(map[string]any)
		name := profile["name"].(string)
		if name <= previous {
			return nil, packageError("profile order")
		}
		previous = name
		v.profiles[name] = profile
	}
	for _, profile := range v.profiles {
		if e := validatePackageProfile(profile, v.entries); e != nil {
			return nil, e
		}
	}
	if e := p.referencesAndUses(); e != nil {
		return nil, e
	}
	if e := p.expandRoots(); e != nil {
		return nil, e
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
	v.ledger.Files = len(v.files)
	v.ledger.Roots = len(v.roots)
	v.ledger.SharedRows = len(v.rows)
	v.ledger.Content = len(v.content)
	if ctx.Err() != nil {
		return nil, packageError("deadline")
	}
	v.semantic = p.semanticView()
	if ctx.Err() != nil || v.semantic == nil {
		return nil, packageError("semantic deadline")
	}
	return v, nil
}

func decodeInto(raw []byte, m *map[string]any) error {
	v, e := decodeStrictDepth(bytes.NewReader(raw), 524288, 32)
	if e != nil {
		return ErrUnavailable
	}
	*m = v.(map[string]any)
	return nil
}
func numberEquals(v any, n int) bool { got, e := packagingInt(v, n); return e == nil && got == n }
func (p *packageCheck) metadata(kind string, raw []byte) (map[string]any, error) {
	if len(raw) == 0 || p.ctx.Err() != nil {
		return nil, packageError("missing metadata")
	}
	v, e := decodeBudget(bytes.NewReader(raw), 262144, 32, &p.budget)
	if e != nil || !bytes.Equal(canonical(v), raw) {
		return nil, packageError(kind + " metadata")
	}
	validation, e := schemaValue(v)
	if e != nil || packageSchemas.byKind[kind].Validate(validation) != nil {
		return nil, packageError(kind + " metadata")
	}
	n, depth := jsonWork(v, 1)
	p.v.ledger.MetadataNodes += n
	if depth > p.v.ledger.MaxDepth {
		p.v.ledger.MaxDepth = depth
	}
	return v.(map[string]any), nil
}
func jsonWork(v any, depth int) (nodes, maxDepth int) {
	nodes = 1
	maxDepth = depth
	switch x := v.(type) {
	case map[string]any:
		for _, v := range x {
			n, d := jsonWork(v, depth+1)
			nodes += n
			if d > maxDepth {
				maxDepth = d
			}
		}
	case []any:
		for _, v := range x {
			n, d := jsonWork(v, depth+1)
			nodes += n
			if d > maxDepth {
				maxDepth = d
			}
		}
	}
	return
}
func sameIdentity(a, b map[string]any) bool {
	if b == nil {
		return false
	}
	projection := map[string]any{}
	for _, k := range []string{"id", "kind", "target", "aliases"} {
		projection[k] = a[k]
	}
	return bytes.Equal(canonical(projection), canonical(b))
}
func entryModule(e map[string]any) string {
	switch e["kind"] {
	case "operation", "schema":
		return e["target"].([]any)[0].(string)
	case "discovery":
		if e["id"] == "octo-cli/discovery/module-map" || e["id"] == "octo-cli/discovery/search-all" {
			return "_global"
		}
		return e["target"].(string)
	case "secret":
		return "bot"
	case "derived":
		return "docs"
	default:
		return "_global"
	}
}
func declaredJSON(c map[string]any) bool {
	m, _ := c["media_type"].(string)
	return m == "application/json" || strings.HasSuffix(m, "+json")
}

func (p *packageCheck) referencesAndUses() error {
	v := p.v
	for _, c := range v.content {
		switch c["kind"] {
		case "bytes", "template":
			field := "data_ref"
			if c["kind"] == "template" {
				field = "value_ref"
			}
			ref := c[field].(string)
			root := p.rootRecords[ref]
			if root == nil {
				return packageError("root type")
			}
			if c["kind"] == "bytes" {
				length, err := inheritedInt(c["length"], 8388608)
				rootLength, err2 := packagingInt(root["length"], 8388608)
				if err != nil || err2 != nil || c["sha256"] != ref || length != rootLength {
					return packageError("bytes/root binding")
				}
			}
			if _, e := packagingInt(root["length"], 8388608); e != nil {
				return packageError("root length")
			}
			p.uses[ref]++
			if declaredJSON(c) {
				p.jsonRoots[ref] = true
			}
		case "schema_variant":
			if v.content[c["template_hash"].(string)]["kind"] != "template" || v.content[c["parameter_truth_hash"].(string)]["kind"] != "bytes" {
				return packageError("variant edge type")
			}
			// Parameter truth is a declared JSON consumer even if another opaque
			// consumer shares its Root. Do not sniff unrelated opaque Roots.
			truth := v.content[c["parameter_truth_hash"].(string)]
			p.jsonRoots[truth["data_ref"].(string)] = true
		case "discovery_index":
			for _, value := range c["rows"].([]any) {
				r := v.rows[value.(string)]
				if r == nil {
					return packageError("row edge type")
				}
				ref := r["row_ref"].(string)
				if p.rootRecords[ref] == nil {
					return packageError("row root type")
				}
				p.uses[ref]++
				p.jsonRoots[ref] = true
			}
		}
	}
	for _, r := range v.rows {
		ref := r["row_ref"].(string)
		if p.rootRecords[ref] == nil {
			return packageError("shared row root")
		}
		p.jsonRoots[ref] = true
	}
	return nil
}

func (p *packageCheck) expandRoots() error {
	v := p.v
	v.ledger.PreNodes = v.ledger.MetadataNodes
	for _, id := range sortedObjectKeys(p.rootRecords) {
		if p.ctx.Err() != nil {
			return packageError("deadline")
		}
		r := p.rootRecords[id]
		n, e := packagingInt(r["length"], 8388608)
		if e != nil {
			return packageError("root integer")
		}
		weight := p.uses[id]
		if weight == 0 {
			weight = 1
		}
		if n > 16777216/weight || addWork(&v.ledger.PreBytes, n*weight, 16777216) != nil {
			return packageError("pre bytes")
		}
		parts := r["parts"].([]any)
		if (n == 0) != (len(parts) == 0) || addWork(&v.ledger.Parts, len(parts), 65536) != nil {
			return packageError("parts")
		}
		if len(parts) > v.ledger.MaxPartsPerRoot {
			v.ledger.MaxPartsPerRoot = len(parts)
		}
		size := 0
		for _, value := range parts {
			part := value.(map[string]any)
			raw := v.files[part["path"].(string)]
			kind, _, _, ok := packagePath(part["path"].(string))
			offset, e := packagingInt(part["offset"], 1048576)
			length, e2 := packagingInt(part["length"], 1048576)
			if !ok || kind != "blob" || e != nil || e2 != nil || length == 0 || offset > len(raw) || length > len(raw)-offset || addWork(&size, length, n) != nil {
				return packageError("part range/type")
			}
		}
		if size != n {
			return packageError("part sum")
		}
		out := make([]byte, 0, n)
		for _, value := range parts {
			part := value.(map[string]any)
			offset, _ := packagingInt(part["offset"], 1048576)
			length, _ := packagingInt(part["length"], 1048576)
			out = append(out, v.files[part["path"].(string)][offset:offset+length]...)
		}
		if hash(out) != id {
			return packageError("raw root hash")
		}
		v.roots[id] = out
		if p.jsonRoots[id] {
			remaining := (1048576 - v.ledger.PreNodes) / weight
			_, nodes, depth, e := rawJSONWork(out, 8388608, 32, remaining, false)
			if e != nil {
				return packageError("declared JSON root")
			}
			p.rootNodes[id] = nodes
			if addWork(&v.ledger.PreNodes, nodes*weight, 1048576) != nil {
				return packageError("pre nodes")
			}
			if depth > v.ledger.MaxDepth {
				v.ledger.MaxDepth = depth
			}
		}
	}
	return nil
}
