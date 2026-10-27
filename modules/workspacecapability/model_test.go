package workspacecapability

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizedStripsConnectorCredentials(t *testing.T) {
	item := ImportItem{
		Kind: KindConnector, Name: "GitHub", Source: SourceLocal, SourceID: "mcp-1",
		Definition: map[string]any{
			"url":     "https://user:pass@example.com/mcp",
			"headers": map[string]any{"Authorization": "Bearer secret"},
			"env":     map[string]any{"TOKEN": "secret"},
		},
		Auth: &Auth{Mode: AuthModeRequired, Status: AuthStatusPending, CredentialKeys: []string{"TOKEN"}},
	}
	resource, err := item.normalized(time.Now(), "u1")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if resource.Definition["url"] != "https://example.com/mcp" {
		t.Fatalf("url was not stripped: %v", resource.Definition["url"])
	}
	headers := resource.Definition["headers"].(map[string]any)
	if headers["Authorization"] != "" {
		t.Fatalf("header value leaked: %v", headers)
	}
	env := resource.Definition["env"].(map[string]any)
	if env["TOKEN"] != "" {
		t.Fatalf("env value leaked: %v", env)
	}
}

func TestNormalizedFilesChangeSnapshot(t *testing.T) {
	item := ImportItem{Kind: KindSkill, Name: "demo", Source: SourceLocal, SourceID: "skill-1", Content: "# body"}
	first, err := item.normalized(time.Now(), "u1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	item.Files = []File{{Path: "scripts/run.sh", Content: "echo hi"}}
	second, err := item.normalized(time.Now(), "u1")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Snapshot == second.Snapshot {
		t.Fatal("snapshot should change when files change")
	}
	if !strings.HasPrefix(second.Snapshot, "sha256:") {
		t.Fatalf("unexpected snapshot: %s", second.Snapshot)
	}
}

func TestNormalizedRejectsInvalidKindAndSource(t *testing.T) {
	if _, err := (ImportItem{Kind: "other", Name: "x", Source: SourceLocal, SourceID: "x", Content: "x"}).normalized(time.Now(), "u"); err == nil {
		t.Fatal("expected invalid kind error")
	}
	if _, err := (ImportItem{Kind: KindSkill, Name: "x", Source: "other", SourceID: "x", Content: "x"}).normalized(time.Now(), "u"); err == nil {
		t.Fatal("expected invalid source error")
	}
}

func TestSanitizeDefinitionRemovesNestedAndQueryCredentials(t *testing.T) {
	item := ImportItem{
		Kind: KindConnector, Name: "secure", Source: SourceLocal, SourceID: "mcp-secure",
		Definition: map[string]any{
			"url": "https://example.com/mcp?access_token=secret&region=us#token",
			"auth": map[string]any{
				"type": "oauth2", "client_secret": "secret",
				"nested": map[string]any{"api_key": "secret", "label": "keep"},
				"list":   []any{map[string]any{"key": "token", "value": "secret"}, "raw-secret"},
			},
			"nested":  map[string]any{"refresh_token": "secret", "region": "us"},
			"headers": map[string]any{"Authorization": "Bearer secret", "X-Region": "us"},
			"env":     map[string]any{"TOKEN": "secret", "MODE": "${MODE}"},
		},
	}
	resource, err := item.normalized(time.Now(), "u1")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := resource.Definition["url"]; got != "https://example.com/mcp?region=us" {
		t.Fatalf("query credentials leaked: %v", got)
	}
	auth := resource.Definition["auth"].(map[string]any)
	if auth["client_secret"] != "" {
		t.Fatalf("object auth leaked: %v", auth)
	}
	nested := auth["nested"].(map[string]any)
	if nested["api_key"] != "" || nested["label"] != "" {
		t.Fatalf("nested auth leaked: %v", nested)
	}
	list := auth["list"].([]any)
	if list[0].(map[string]any)["value"] != "" || list[1] != "" {
		t.Fatalf("auth array leaked: %v", list)
	}
	nestedConfig := resource.Definition["nested"].(map[string]any)
	if nestedConfig["refresh_token"] != "" || nestedConfig["region"] != "us" {
		t.Fatalf("nested config sanitization failed: %v", nestedConfig)
	}
	headers := resource.Definition["headers"].(map[string]any)
	if headers["Authorization"] != "" || headers["X-Region"] != "" {
		t.Fatalf("header values leaked: %v", headers)
	}
	env := resource.Definition["env"].(map[string]any)
	if env["TOKEN"] != "" || env["MODE"] != "${MODE}" {
		t.Fatalf("env values wrong: %v", env)
	}
}

func TestNormalizedRejectsInvalidFiles(t *testing.T) {
	base := ImportItem{Kind: KindSkill, Name: "files", Source: SourceLocal, SourceID: "files-1", Content: "# body"}
	cases := []struct {
		name  string
		files []File
	}{
		{"duplicate", []File{{Path: "a.md", Content: "1"}, {Path: "a.md", Content: "2"}}},
		{"traversal", []File{{Path: "../a.md", Content: "1"}}},
		{"absolute", []File{{Path: "/a.md", Content: "1"}}},
		{"empty path", []File{{Path: " ", Content: "1"}}},
		{"oversized", []File{{Path: "a.md", Content: strings.Repeat("x", maxFileBytes+1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := base
			item.Files = tc.files
			if _, err := item.normalized(time.Now(), "u1"); err == nil {
				t.Fatal("expected validation error")
			} else {
				var validation *ValidationError
				if !errors.As(err, &validation) {
					t.Fatalf("expected ValidationError, got %T", err)
				}
			}
		})
	}
}

func TestNormalizedKeepsValidFiles(t *testing.T) {
	item := ImportItem{Kind: KindSkill, Name: "files", Source: SourceLocal, SourceID: "files-1", Content: "# body",
		Files: []File{{Path: "scripts/run.sh", Content: "echo hi"}}}
	resource, err := item.normalized(time.Now(), "u1")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(resource.Files) != 1 || resource.Files[0].Path != "scripts/run.sh" {
		t.Fatalf("files were dropped: %+v", resource.Files)
	}
}

func TestValidScopeIDRejectsWhitespacePaddedIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"space_123", true},
		{"Space-A", true},
		{"space_123 ", false},
		{" space_123", false},
		{"space_123\t", false},
		{"space_123\x00", false},
		{"space_123\u00a0", false},
		{"", false},
		{strings.Repeat("a", 65), false},
	} {
		if got := validScopeID(tc.value); got != tc.ok {
			t.Fatalf("validScopeID(%q) = %v, want %v", tc.value, got, tc.ok)
		}
	}
}

func TestNormalizedRejectsMalformedConnectorURLs(t *testing.T) {
	for _, raw := range []string{
		"https://user:example-password@example.invalid:bad/mcp?token=example-token",
		"https://example.com/%zz?token=example-token",
	} {
		item := ImportItem{Kind: KindConnector, Name: "bad-url", Source: SourceLocal, SourceID: "bad-url", Definition: map[string]any{"url": raw}}
		_, err := item.normalized(time.Now(), "u1")
		if err == nil {
			t.Fatalf("expected validation error for %q", raw)
		}
		var validation *ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("expected ValidationError, got %T", err)
		}
		if strings.Contains(err.Error(), "example-password") || strings.Contains(err.Error(), "example-token") {
			t.Fatalf("validation error echoed credentials: %v", err)
		}
	}
}
