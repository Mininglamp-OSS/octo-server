package permissionbundle

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
)

const (
	SourceSlotWitnessFormatV1 = "permission-source-slot-witness-v1"
	SourceSlotWitnessStatus   = "SOURCE_EVIDENCE_NOT_SUPPORT_APPROVAL"
)

// SlotWitnessLedger records verified byte/digest/cell provenance for an
// external source-slot witness artifact. Matching a witness or verifying a
// candidate package is source evidence only and never constitutes C1-C9 or
// production allowlist approval.
type SlotWitnessLedger struct {
	CompressedBytes   int        `json:"compressed_bytes,omitempty"`
	CompressedSHA256  string     `json:"compressed_sha256,omitempty"`
	DecodedBytes      int        `json:"decoded_bytes"`
	DecodedSHA256     string     `json:"decoded_sha256"`
	CatalogPin        CatalogPin `json:"catalog_pin"`
	SourceCLI         string     `json:"source_cli"`
	SchemaSourceCells int        `json:"schema_source_cells"`
	SlotBearingCells  int        `json:"slot_bearing_cells"`
	Groups            int        `json:"groups"`
	Status            string     `json:"status"`
}

// ReadSlotWitnessFile reads a regular non-aliased gzip slot witness file and
// validates its compressed/decoded digests and finite cell assignments.
func ReadSlotWitnessFile(pin CatalogPin, path, expectedCompressedSHA256 string) (SlotValues, SlotWitnessLedger, error) {
	raw, err := readRegularFile(path, 1048576)
	if err != nil {
		return nil, SlotWitnessLedger{}, packageError("slot witness file")
	}
	return LoadSlotWitnessGzip(pin, raw, expectedCompressedSHA256)
}

// LoadSlotWitnessGzip decompresses and validates a single-stream gzip source
// slot witness bound to pin.
func LoadSlotWitnessGzip(pin CatalogPin, compressed []byte, expectedCompressedSHA256 string) (SlotValues, SlotWitnessLedger, error) {
	if len(compressed) == 0 || len(compressed) > 1048576 {
		return nil, SlotWitnessLedger{}, packageError("slot witness compressed bounds")
	}
	compressedSHA := hash(compressed)
	if expectedCompressedSHA256 != "" && (!validHex(expectedCompressedSHA256, 64) || compressedSHA != expectedCompressedSHA256) {
		return nil, SlotWitnessLedger{}, packageError("slot witness compressed digest")
	}
	reader := bytes.NewReader(compressed)
	zr, err := gzip.NewReader(reader)
	if err != nil {
		return nil, SlotWitnessLedger{}, packageError("slot witness gzip header")
	}
	zr.Multistream(false)
	decoded, err := io.ReadAll(io.LimitReader(zr, 8388608+1))
	if err != nil || zr.Close() != nil || reader.Len() != 0 || len(decoded) == 0 || len(decoded) > 8388608 {
		return nil, SlotWitnessLedger{}, packageError("slot witness gzip stream")
	}
	slots, ledger, err := LoadSlotWitnessJSON(pin, decoded, "")
	if err != nil {
		return nil, SlotWitnessLedger{}, err
	}
	ledger.CompressedBytes = len(compressed)
	ledger.CompressedSHA256 = compressedSHA
	return slots, ledger, nil
}

// LoadSlotWitnessJSON validates a canonical JSON source-slot witness bound to
// pin and returns a detached SlotValues callback over its finite cell partition.
func LoadSlotWitnessJSON(pin CatalogPin, rawJSON []byte, expectedDecodedSHA256 string) (SlotValues, SlotWitnessLedger, error) {
	loadPackageSchemas()
	if packageSchemas.err != nil || !validHex(pin.Digest, 64) || !validCatalogVersion(pin.Version) || len(rawJSON) == 0 || len(rawJSON) > 8388608 {
		return nil, SlotWitnessLedger{}, packageError("slot witness input")
	}
	decodedSHA := hash(rawJSON)
	if expectedDecodedSHA256 != "" && (!validHex(expectedDecodedSHA256, 64) || decodedSHA != expectedDecodedSHA256) {
		return nil, SlotWitnessLedger{}, packageError("slot witness decoded digest")
	}
	budget := 1048576
	v, err := decodeBudget(bytes.NewReader(rawJSON), 8388608, 32, &budget)
	if err != nil || !bytes.Equal(canonical(v), rawJSON) {
		return nil, SlotWitnessLedger{}, packageError("slot witness canonical json")
	}
	m, ok := v.(map[string]any)
	if !ok || !exactKeys(m, "format", "catalog_format", "catalog_version", "manifest_sha256", "source_cli", "schema_source_cells", "slot_bearing_cells", "server_facades", "groups", "status", "static") {
		return nil, SlotWitnessLedger{}, packageError("slot witness top-level shape")
	}
	sourceCLI, _ := m["source_cli"].(string)
	staticNote, _ := m["static"].(string)
	status, _ := m["status"].(string)
	if m["format"] != SourceSlotWitnessFormatV1 ||
		m["catalog_format"] != CatalogPackageV2 ||
		m["catalog_version"] != pin.Version ||
		m["manifest_sha256"] != pin.Digest ||
		!validHex(sourceCLI, 40) ||
		status != SourceSlotWitnessStatus ||
		staticNote == "" {
		return nil, SlotWitnessLedger{}, packageError("slot witness header")
	}
	schemaCells, err := packagingInt(m["schema_source_cells"], 56592)
	if err != nil || schemaCells != 56592 {
		return nil, SlotWitnessLedger{}, packageError("slot witness schema cell count")
	}
	bearingCells, err := packagingInt(m["slot_bearing_cells"], 56592)
	if err != nil || bearingCells <= 0 || bearingCells > 56592 {
		return nil, SlotWitnessLedger{}, packageError("slot witness bearing cell count")
	}
	facades, ok := m["server_facades"].([]any)
	if !ok || len(facades) != len(serverFacades) {
		return nil, SlotWitnessLedger{}, packageError("slot witness server facades")
	}
	for i, want := range serverFacades {
		if facades[i] != want {
			return nil, SlotWitnessLedger{}, packageError("slot witness server facades")
		}
	}
	groups, ok := m["groups"].([]any)
	if !ok || len(groups) == 0 || len(groups) > bearingCells {
		return nil, SlotWitnessLedger{}, packageError("slot witness groups")
	}
	assignments := make(map[string]map[string]any, bearingCells)
	for _, item := range groups {
		g, ok := item.(map[string]any)
		if !ok || !exactKeys(g, "entry_id", "facade", "selector", "values") {
			return nil, SlotWitnessLedger{}, packageError("slot witness group shape")
		}
		entryID, _ := g["entry_id"].(string)
		ident := packageSchemas.identities[entryID]
		if ident == nil || ident["kind"] != "schema" {
			return nil, SlotWitnessLedger{}, packageError("slot witness group entry")
		}
		facade, _ := g["facade"].(string)
		if facade != "describe_op" && facade != "get_skill_full" && facade != "get_skill_summary" {
			return nil, SlotWitnessLedger{}, packageError("slot witness group facade")
		}
		selector, ok := g["selector"].(map[string]any)
		if !ok || !exactKeys(selector, "resources_bits", "managed_bits", "facade_bits") {
			return nil, SlotWitnessLedger{}, packageError("slot witness group selector")
		}
		tuples, err := selectorTuples(selector, true)
		if err != nil {
			return nil, SlotWitnessLedger{}, err
		}
		values, ok := g["values"].(map[string]any)
		if !ok || len(values) == 0 || len(values) > 2 {
			return nil, SlotWitnessLedger{}, packageError("slot witness group values")
		}
		for domain, val := range values {
			if domain != "facade_hint" && domain != "server_managed_argument_names" {
				return nil, SlotWitnessLedger{}, packageError("slot witness domain")
			}
			if _, err := slotReplacement(domain, val); err != nil {
				return nil, SlotWitnessLedger{}, err
			}
		}
		cloned := cloneSlotValuesMap(values)
		for _, t := range tuples {
			key := cellKey(entryID, facade, t[0], t[1], t[2])
			if assignments[key] != nil {
				return nil, SlotWitnessLedger{}, packageError("slot witness overlap")
			}
			assignments[key] = cloned
		}
	}
	if len(assignments) != bearingCells {
		return nil, SlotWitnessLedger{}, packageError("slot witness coverage count")
	}
	ledger := SlotWitnessLedger{
		DecodedBytes:      len(rawJSON),
		DecodedSHA256:     decodedSHA,
		CatalogPin:        pin,
		SourceCLI:         sourceCLI,
		SchemaSourceCells: schemaCells,
		SlotBearingCells:  bearingCells,
		Groups:            len(groups),
		Status:            status,
	}
	callback := func(ctx context.Context, cell SlotCell) (map[string]any, error) {
		if ctx.Err() != nil {
			return nil, packageError("deadline")
		}
		if cell.Pin != pin || cell.ManagedMask < 0 || cell.ManagedMask > 7 {
			return nil, packageError("slot witness cell pin/mask")
		}
		r := 0
		if cell.Resources {
			r = 1
		}
		f := -1
		for i, name := range serverFacades {
			if cell.ServerFacade == name {
				f = i
				break
			}
		}
		if f < 0 {
			return nil, packageError("slot witness server facade")
		}
		vals, ok := assignments[cellKey(cell.EntryID, cell.Facade, r, cell.ManagedMask, f)]
		if !ok {
			return nil, packageError("missing slot witness cell")
		}
		return cloneSlotValuesMap(vals), nil
	}
	return callback, ledger, nil
}

func exactKeys(m map[string]any, keys ...string) bool {
	if len(m) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}

func cloneSlotValuesMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch x := v.(type) {
		case []any:
			cp := make([]any, len(x))
			copy(cp, x)
			out[k] = cp
		case []string:
			cp := make([]string, len(x))
			copy(cp, x)
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

func validCatalogVersion(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func readRegularFile(path string, limit int) ([]byte, error) {
	if path == "" {
		return nil, ErrUnavailable
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	initialDir, err := os.Lstat(dir)
	if err != nil || !initialDir.IsDir() || initialDir.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnavailable
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer root.Close()
	dirInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(initialDir, dirInfo) {
		return nil, ErrUnavailable
	}
	stat, err := root.Lstat(base)
	if err != nil || !stat.Mode().IsRegular() || !singleLink(stat) || stat.Size() <= 0 || stat.Size() > int64(limit) {
		return nil, ErrUnavailable
	}
	f, err := root.Open(base)
	if err != nil {
		return nil, ErrUnavailable
	}
	opened, err := f.Stat()
	if err != nil || !stableFile(stat, opened) {
		f.Close()
		return nil, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	after, err2 := f.Stat()
	closeErr := f.Close()
	if err != nil || err2 != nil || closeErr != nil || len(raw) != int(stat.Size()) || !stableFile(stat, after) {
		return nil, ErrUnavailable
	}
	return raw, nil
}
