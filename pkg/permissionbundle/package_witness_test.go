package permissionbundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func cliCandidatePackageV5(t *testing.T) (PackageInput, map[string]any) {
	t.Helper()
	f, err := os.Open("testdata/catalog-package-v2-candidate-v5.tar.gz")
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer zr.Close()
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.Equal(t, byte(tar.TypeReg), hdr.Typeflag)
		raw, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[hdr.Name] = raw
	}
	require.Len(t, files, 30)
	manifestBytes := files["manifest.json"]
	require.Len(t, manifestBytes, 4934)
	manifestSHA := independentHash(manifestBytes)
	require.Equal(t, "ecc56c2ab0a2c520ab7e5203d2abab06671daf90c28d69f88fe1de8d154109fc", manifestSHA)
	manifestObj := fixtureObject(t, manifestBytes)

	reportRaw, err := os.ReadFile("testdata/catalog-package-report-v5.json")
	require.NoError(t, err)
	require.Len(t, reportRaw, 1914)
	require.Equal(t, "7257dfef2ed8d48844f9a97335926109f9a97335926109f4b49e85b4eb233ba593202375e85caf"[0:16], independentHash(reportRaw)[0:16])
	require.Equal(t, "7257dfef2ed8d48844f9a97335926109f4b49e85b4eb233ba593202375e85caf", independentHash(reportRaw))
	reportObj := fixtureObject(t, reportRaw)
	return PackageInput{
		Pin:   CatalogPin{Version: manifestObj["catalog_version"].(string), Digest: manifestSHA},
		Files: files,
	}, reportObj
}

func TestCLICandidatePackageV2AndSlotWitnessV5(t *testing.T) {
	input, report := cliCandidatePackageV5(t)
	require.Equal(t, "octo-capabilities-v1-xin2748-v4", input.Pin.Version)
	require.Equal(t, "ecc56c2ab0a2c520ab7e5203d2abab06671daf90c28d69f88fe1de8d154109fc", input.Pin.Digest)

	const expectedWitnessCompressedSHA = "0c5da173bd0f53c64562a70a5c5feebc955ad111bbe84ed11f5588c08b4b9c0b"
	const expectedWitnessDecodedSHA = "0dac894681aa7f4f8810f5fa5574d68ce071f2a401df4a99da4a92e7a43e7c5b"
	const expectedSemanticDigest = "15c7ca2bf6df703979df529afbca6828f116b2463b1a818456edfad80bcad799"

	slots, witnessLedger, err := ReadSlotWitnessFile(input.Pin, "testdata/source-slot-witness-v5.json.gz", expectedWitnessCompressedSHA)
	require.NoError(t, err)
	require.Equal(t, 12562, witnessLedger.CompressedBytes)
	require.Equal(t, expectedWitnessCompressedSHA, witnessLedger.CompressedSHA256)
	require.Equal(t, 843132, witnessLedger.DecodedBytes)
	require.Equal(t, expectedWitnessDecodedSHA, witnessLedger.DecodedSHA256)
	require.Equal(t, input.Pin, witnessLedger.CatalogPin)
	require.Equal(t, "f8d434b6318215948ebbf67feb11f6265dee11e5", witnessLedger.SourceCLI)
	require.Equal(t, 56592, witnessLedger.SchemaSourceCells)
	require.Equal(t, 29070, witnessLedger.SlotBearingCells)
	require.Equal(t, 2700, witnessLedger.Groups)
	require.Equal(t, SourceSlotWitnessStatus, witnessLedger.Status)

	verified, err := VerifyCatalogPackage(t.Context(), input, slots)
	require.NoError(t, err)
	require.Equal(t, expectedSemanticDigest, verified.SemanticDigest())
	require.Equal(t, report["semantic_sha256"], verified.SemanticDigest())

	ledger := verified.Ledger()
	require.Equal(t, 7823327, ledger.StoredBytes)
	require.Equal(t, 10202896, ledger.PreBytes)
	require.Equal(t, 11603961, ledger.PostBytes)
	require.Equal(t, 158934, ledger.MetadataNodes)
	require.Equal(t, 399778, ledger.PreNodes)
	require.Equal(t, 398617, ledger.PostNodes)
	require.Equal(t, 15, ledger.MaxDepth)
	require.Equal(t, 30, ledger.Files)
	require.Equal(t, 4128, ledger.Roots)
	require.Equal(t, 1179, ledger.SharedRows)
	require.Equal(t, 5481, ledger.Content)
	require.Equal(t, 15649, ledger.Parts)
	require.Equal(t, 25, ledger.MaxPartsPerRoot)
	require.Equal(t, 56592, ledger.SchemaCells)
	require.Equal(t, 156, ledger.DiscoveryCells)
	require.Equal(t, 1248, ledger.StaticCells)

	reportLedger := report["ledger"].(map[string]any)
	actualLedger := fixtureObject(t, fixtureJSON(t, ledger))
	for _, key := range []string{"stored_bytes", "pre_bytes", "post_bytes", "metadata_nodes", "pre_nodes", "post_nodes", "max_depth", "files", "roots", "shared_rows", "content_records", "max_parts_per_root", "schema_cells", "discovery_cells", "static_cells"} {
		require.Equal(t, reportLedger[key], actualLedger[key], key)
	}
	require.Equal(t, reportLedger["parts_total"], actualLedger["parts"])

	// Verify ReadCatalogDirectory on the 30-file candidate staging layout.
	stagingDir := t.TempDir()
	for rel, raw := range input.Files {
		full := filepath.Join(stagingDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, raw, 0o600))
	}
	dirInput, err := ReadCatalogDirectory(t.Context(), input.Pin, stagingDir)
	require.NoError(t, err)
	dirVerified, err := VerifyCatalogPackage(t.Context(), dirInput, slots)
	require.NoError(t, err)
	require.True(t, semanticEqual(verified, dirVerified))
	require.Equal(t, expectedSemanticDigest, dirVerified.SemanticDigest())
}

func TestSlotWitnessLoaderRejectsCorruptOrMismatchedInputs(t *testing.T) {
	input, _ := cliCandidatePackageV5(t)
	compressed, err := os.ReadFile("testdata/source-slot-witness-v5.json.gz")
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	require.NoError(t, err)
	decoded, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())

	t.Run("wrong_compressed_digest", func(t *testing.T) {
		_, _, err := LoadSlotWitnessGzip(input.Pin, compressed, independentHash([]byte("wrong")))
		require.ErrorIs(t, err, ErrUnavailable)
	})
	t.Run("trailing_gzip_bytes", func(t *testing.T) {
		withTrailing := append(append([]byte(nil), compressed...), 'x')
		_, _, err := LoadSlotWitnessGzip(input.Pin, withTrailing, "")
		require.ErrorIs(t, err, ErrUnavailable)
	})
	t.Run("wrong_pin", func(t *testing.T) {
		_, _, err := LoadSlotWitnessJSON(CatalogPin{Version: input.Pin.Version, Digest: independentHash([]byte("other"))}, decoded, "")
		require.ErrorIs(t, err, ErrUnavailable)
	})
	t.Run("non_canonical_json", func(t *testing.T) {
		_, _, err := LoadSlotWitnessJSON(input.Pin, append([]byte(" "), decoded...), "")
		require.ErrorIs(t, err, ErrUnavailable)
	})
	t.Run("overlapping_selector_group", func(t *testing.T) {
		obj := fixtureObject(t, decoded)
		groups := obj["groups"].([]any)
		obj["groups"] = append([]any{groups[0]}, groups...)
		raw := canonical(obj)
		_, _, err := LoadSlotWitnessJSON(input.Pin, raw, "")
		require.ErrorContains(t, err, "slot witness")
	})
	t.Run("symlink_witness_file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json.gz")
		link := filepath.Join(dir, "link.json.gz")
		require.NoError(t, os.WriteFile(target, compressed, 0o600))
		require.NoError(t, os.Symlink(target, link))
		_, _, err := ReadSlotWitnessFile(input.Pin, link, "")
		require.ErrorIs(t, err, ErrUnavailable)
	})
}
