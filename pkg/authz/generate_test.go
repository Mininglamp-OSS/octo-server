package authz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestGenerateArtifactsDeterministic(t *testing.T) {
	manifest := repositoryManifest(t)
	goOne, jsonOne, err := GenerateArtifacts(manifest)
	if err != nil {
		t.Fatalf("GenerateArtifacts() error = %v", err)
	}
	goTwo, jsonTwo, err := GenerateArtifacts(manifest)
	if err != nil {
		t.Fatalf("GenerateArtifacts() second error = %v", err)
	}
	if !bytes.Equal(goOne, goTwo) || !bytes.Equal(jsonOne, jsonTwo) {
		t.Fatal("identical manifest did not produce byte-identical outputs")
	}
	if !bytes.Contains(goOne, []byte("Code generated")) {
		t.Fatal("generated Go file lacks generated-file marker")
	}
}

func TestGeneratedContractCollectionsAreSorted(t *testing.T) {
	manifest := repositoryManifest(t)
	_, output, err := GenerateArtifacts(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var contract generatedContract
	if err := json.Unmarshal(output, &contract); err != nil {
		t.Fatal(err)
	}
	if !sort.SliceIsSorted(contract.Permissions, func(i, j int) bool { return contract.Permissions[i].Key < contract.Permissions[j].Key }) {
		t.Error("permissions are not sorted by key")
	}
	if !sort.SliceIsSorted(contract.LegacyCapabilities, func(i, j int) bool { return contract.LegacyCapabilities[i].Key < contract.LegacyCapabilities[j].Key }) {
		t.Error("legacy capabilities are not sorted by key")
	}
	if !sort.SliceIsSorted(contract.GateSites, func(i, j int) bool { return contract.GateSites[i].Source < contract.GateSites[j].Source }) {
		t.Error("gate sites are not sorted by source identity")
	}
	if !sort.SliceIsSorted(contract.Operations, func(i, j int) bool { return contract.Operations[i].ID < contract.Operations[j].ID }) {
		t.Error("operations are not sorted by ID")
	}
}

func TestGeneratedContractDefinesAggregateModeSemantics(t *testing.T) {
	manifest := repositoryManifest(t)
	_, output, err := GenerateArtifacts(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var contract generatedContract
	if err := json.Unmarshal(output, &contract); err != nil {
		t.Fatal(err)
	}
	if got, want := contract.AggregateModeSemantics.Any, AggregateAnySemantics; got != want {
		t.Fatalf("aggregate any semantics = %q, want %q", got, want)
	}
	if got, want := contract.AggregateModeSemantics.All, AggregateAllSemantics; got != want {
		t.Fatalf("aggregate all semantics = %q, want %q", got, want)
	}
}

func TestGeneratedJSONHashExcludesItselfAndDynamicAuthorizationState(t *testing.T) {
	manifest := repositoryManifest(t)
	_, output, err := GenerateArtifacts(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"users", "roles", "bindings", "authorization_revision"} {
		if _, exists := decoded[forbidden]; exists {
			t.Errorf("generated contract contains forbidden dynamic field %q", forbidden)
		}
	}
	var generated generatedContract
	if err := json.Unmarshal(output, &generated); err != nil {
		t.Fatal(err)
	}
	hash := generated.ContentHash
	generated.ContentHash = ""
	canonical, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256Hex(canonical)
	if hash != want {
		t.Fatalf("content hash = %q, want %q", hash, want)
	}
}

func TestWriteGeneratedFilesDoesNotOverwriteOnInvalidManifest(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "manifest.yaml")
	goPath := filepath.Join(directory, "permissions.go")
	jsonPath := filepath.Join(directory, "permissions.json")
	oldGo, oldJSON := []byte("old go"), []byte("old json")
	if err := os.WriteFile(goPath, oldGo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, oldJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(emptyManifest, "schema_version: 1", "schema_version: 99", 1)
	if err := os.WriteFile(manifestPath, []byte(invalid), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteGeneratedFiles(manifestPath, goPath, jsonPath); err == nil {
		t.Fatal("WriteGeneratedFiles() accepted invalid manifest")
	}
	gotGo, _ := os.ReadFile(goPath)
	gotJSON, _ := os.ReadFile(jsonPath)
	if !bytes.Equal(gotGo, oldGo) || !bytes.Equal(gotJSON, oldJSON) {
		t.Fatal("invalid manifest overwrote an existing generated file")
	}
}

func TestCheckGeneratedFilesRejectsManualDrift(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(repositoryRoot(t), "authz", "manager-permissions.yaml")
	goPath := filepath.Join(directory, "permissions.go")
	jsonPath := filepath.Join(directory, "permissions.json")
	if err := WriteGeneratedFiles(manifestPath, goPath, jsonPath); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(jsonPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckGeneratedFiles(manifestPath, goPath, jsonPath); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("CheckGeneratedFiles() error = %v, want drift", err)
	}
}

func repositoryManifest(t *testing.T) *Manifest {
	t.Helper()
	manifest, err := LoadManifest(filepath.Join(repositoryRoot(t), "authz", "manager-permissions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func sha256Hex(contents []byte) string {
	hash := sha256.Sum256(contents)
	return hex.EncodeToString(hash[:])
}
