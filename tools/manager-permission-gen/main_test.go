package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
)

func TestCheckContractRunsAllValidationLayers(t *testing.T) {
	root := toolRepositoryRoot(t)
	manifestPath := filepath.Join(root, "authz", "manager-permissions.yaml")
	goPath := filepath.Join(root, "pkg", "authz", "permissions_generated.go")
	jsonPath := filepath.Join(root, "authz", "generated", "manager-permissions.json")
	if err := checkContract(root, manifestPath, goPath, jsonPath); err != nil {
		t.Fatalf("checkContract() valid repository error = %v", err)
	}

	t.Run("structure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.yaml")
		if err := os.WriteFile(path, []byte("schema_version: 1\nunknown: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkContract(root, path, goPath, jsonPath); err == nil {
			t.Fatal("checkContract() accepted invalid manifest structure")
		}
	})

	t.Run("source inventory", func(t *testing.T) {
		if err := checkContract(t.TempDir(), manifestPath, goPath, jsonPath); err == nil {
			t.Fatal("checkContract() accepted missing source inventory")
		}
	})

	t.Run("route inventory", func(t *testing.T) {
		contents, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		mutated := strings.Replace(string(contents), "path: /v1/admin/app_bot\n", "path: /v1/admin/app_bot-missing\n", 1)
		path := filepath.Join(t.TempDir(), "route-drift.yaml")
		if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
			t.Fatal(err)
		}
		err = checkContract(root, path, goPath, jsonPath)
		if err == nil || !strings.Contains(err.Error(), "unregistered global route") {
			t.Fatalf("checkContract() route drift error = %v", err)
		}
	})

	t.Run("generated drift", func(t *testing.T) {
		directory := t.TempDir()
		tempGo := filepath.Join(directory, "permissions.go")
		tempJSON := filepath.Join(directory, "permissions.json")
		if err := authz.WriteGeneratedFiles(manifestPath, tempGo, tempJSON); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tempJSON, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := checkContract(root, manifestPath, tempGo, tempJSON)
		if err == nil || !strings.Contains(err.Error(), "drift") {
			t.Fatalf("checkContract() drift error = %v", err)
		}
	})
}

func toolRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
