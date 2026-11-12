package permissionbundle

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogValueDepthAndAggregateNodeBoundary(t *testing.T) {
	for _, depth := range []int{32, 33} {
		raw := strings.Repeat("[", depth-1) + "0" + strings.Repeat("]", depth-1)
		budget := 1048576
		_, e := decodeBudget(strings.NewReader(raw), 8388608, 32, &budget)
		if depth == 32 {
			require.NoError(t, e)
		} else {
			require.Error(t, e)
		}
	}
	// One array node plus 1,048,575 scalar nodes: exact selected bound.
	raw := []byte("[" + strings.Repeat("null,", 1048574) + "null]")
	budget := 1048576
	v, e := decodeBudget(bytes.NewReader(raw), 8388608, 32, &budget)
	require.NoError(t, e)
	require.Zero(t, budget)
	require.Len(t, v.([]any), 1048575)
	budget = 1048575
	_, e = decodeBudget(bytes.NewReader(raw), 8388608, 32, &budget)
	require.Error(t, e)
	budget = 3
	_, e = decodeBudget(strings.NewReader(`{"one":null}`), 262144, 32, &budget)
	require.NoError(t, e)
	_, e = decodeBudget(strings.NewReader(`[true]`), 262144, 32, &budget)
	require.Error(t, e, "metadata files share the remaining value budget")
}

func TestCatalogRawJSONUsesRendererDomainWithoutMetadataFilters(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"dup":1,"dup":null,"escaped":"\ud800"}`), []byte(`[null,1e0,"\udfff"]`), {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}} {
		_, nodes, _, e := rawJSONWork(raw, 8388608, 32, 1048576, false)
		require.NoError(t, e)
		require.Greater(t, nodes, 1)
		rendered, e := json.MarshalIndent(json.RawMessage(raw), "", "  ")
		require.NoError(t, e)
		require.Equal(t, len(rendered), renderedSize(raw))
		c := map[string]any{"slots": []any{}}
		spans, e := templateSpans(raw, c)
		require.NoError(t, e)
		actual, e := renderPackageTemplate(raw, c, spans, nil)
		require.NoError(t, e)
		require.Equal(t, rendered, actual)
		_, e = decodeStrictDepth(bytes.NewReader(raw), 262144, 32)
		require.Error(t, e, "metadata remains scalar-valid and duplicate-free")
	}
	raw := []byte(`{"a":{"$octo_slot":"id"},"a":{"$octo_slot":"id"}}`)
	_, e := templateSpans(raw, map[string]any{"slots": []any{map[string]any{"id": "id", "pointer": "/a"}}})
	require.ErrorContains(t, e, "ambiguous slot pointer")
}

func TestCatalogRepeatedUseExpansionBoundaryBeforeAllocation(t *testing.T) {
	raw := bytes.Repeat([]byte{'a'}, 1048576)
	id := independentHash(raw)
	makeCheck := func() *packageCheck {
		v := &VerifiedCatalog{files: map[string][]byte{"blobs/0.bin": raw}, roots: map[string][]byte{}}
		return &packageCheck{v: v, ctx: t.Context(), rootRecords: map[string]map[string]any{id: {"length": json.Number("1048576"), "parts": []any{map[string]any{"path": "blobs/0.bin", "offset": json.Number("0"), "length": json.Number("1048576")}}}}, uses: map[string]int{id: 16}, jsonRoots: map[string]bool{}, rootNodes: map[string]int{}}
	}
	p := makeCheck()
	require.NoError(t, p.expandRoots())
	require.Equal(t, 16777216, p.v.ledger.PreBytes)
	p = makeCheck()
	small := []byte{'z'}
	other := independentHash(small)
	p.v.files["blobs/1.bin"] = small
	p.rootRecords[other] = map[string]any{"length": json.Number("1"), "parts": []any{map[string]any{"path": "blobs/1.bin", "offset": json.Number("0"), "length": json.Number("1")}}}
	p.uses[other] = 1
	require.ErrorContains(t, p.expandRoots(), "pre bytes")
}

func TestCatalogNewIntegerDomainDoesNotReplaceInheritedDomain(t *testing.T) {
	for _, number := range []json.Number{"1", "1.0", "1e0"} {
		n, e := inheritedInt(number, 65536)
		require.NoError(t, e)
		require.Equal(t, 1, n)
		_, e = packagingInt(number, 65536)
		if number == "1" {
			require.NoError(t, e)
		} else {
			require.Error(t, e)
		}
	}
	_, e := packagingInt(json.Number("18446744073709551616"), 1048576)
	require.Error(t, e)
	_, e = boundedInteger("1e1000000000", 9007199254740991)
	require.Error(t, e)
	_, e = boundedInteger("1e-1000000000", 9007199254740991)
	require.Error(t, e)
	n, e := boundedInteger("0e1000000000", 9007199254740991)
	require.NoError(t, e)
	require.Zero(t, n)
	v, e := schemaValue(map[string]any{"literal": nil, "number": json.Number("0e1000000000")})
	require.NoError(t, e)
	require.Equal(t, map[string]any{"literal": nil, "number": json.Number("0")}, v)
}

func TestCatalogTypedTruthDoesNotBecomeOpaqueAndSlotsDoNotNormalize(t *testing.T) {
	binary := []byte{0xff}
	truth := independentHash(binary)
	template := independentHash([]byte("{}"))
	blob := append(binary, []byte("{}")...)
	v := &VerifiedCatalog{files: map[string][]byte{"blobs/0.bin": blob}, roots: map[string][]byte{}, rows: map[string]map[string]any{}, content: map[string]map[string]any{
		"truth":    {"kind": "bytes", "data_ref": truth, "sha256": truth, "length": json.Number("1"), "media_type": "application/octet-stream"},
		"template": {"kind": "template", "value_ref": template, "media_type": "application/json"},
		"variant":  {"kind": "schema_variant", "template_hash": "template", "parameter_truth_hash": "truth"},
	}}
	part := func(offset, length string) []any {
		return []any{map[string]any{"path": "blobs/0.bin", "offset": json.Number(offset), "length": json.Number(length)}}
	}
	p := &packageCheck{v: v, ctx: t.Context(), rootRecords: map[string]map[string]any{truth: {"length": json.Number("1"), "parts": part("0", "1")}, template: {"length": json.Number("2"), "parts": part("1", "2")}}, uses: map[string]int{}, jsonRoots: map[string]bool{}, rootNodes: map[string]int{}}
	require.NoError(t, p.referencesAndUses())
	require.ErrorContains(t, p.expandRoots(), "declared JSON root")
	_, e := slotReplacement("server_managed_argument_names", []string{"a\xff"})
	require.ErrorContains(t, e, "managed names scalar")
}
