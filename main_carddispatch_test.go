package main

import (
	"os"
	"strings"
	"testing"
)

func TestCardDispatchRegistryInstalledBeforeModuleConstruction(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(source)
	install := strings.Index(text, "installCardDispatch(ctx)")
	setup := strings.Index(text, "module.Setup(ctx)")
	if install < 0 {
		t.Fatal("main must install the per-context card dispatch registry")
	}
	if setup < 0 || install > setup {
		t.Fatal("card dispatch registry must be installed before module.Setup constructs producers")
	}
}

func TestDormantFoundationRegistersNoProductionProducer(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(source), "carddispatch.NewRegistry(deps, nil)") {
		t.Fatal("foundation rollout must install an empty registry until cross-repo enablement gates pass")
	}
}
