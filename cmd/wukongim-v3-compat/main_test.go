package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefusesPublicUnsignedCallbackListener(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"--webhook-listen=0.0.0.0:5004"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unsafe listener accepted: %v", err)
	}
}

func TestSecretFileBoundsAndRedactedErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(strings.Repeat("s", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(p); err == nil {
		t.Fatal("unbounded secret read")
	}
	if err := os.WriteFile(p, []byte("  private-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecret(p)
	if err != nil || got != "private-secret" {
		t.Fatal("secret whitespace not trimmed")
	}
	_, err = readSecret(p + "-private-secret")
	if err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("error leaked secret file path")
	}
}
