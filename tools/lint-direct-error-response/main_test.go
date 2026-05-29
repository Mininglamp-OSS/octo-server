package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, src string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCountSites(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "modules/a/api.go", `package a
func h(c *Ctx) {
	c.AbortWithStatusJSON(401, nil)
	c.AbortWithStatus(403)
	c.JSON(200, nil) // not counted
}
`)
	// _test.go must be skipped entirely.
	writeFile(t, dir, "modules/a/api_test.go", `package a
func TestX() { c.AbortWithStatusJSON(500, nil) }
`)
	// A file with no direct responses must not appear in the map.
	writeFile(t, dir, "modules/b/clean.go", `package b
func ok(c *Ctx) { c.RenderError(spec) }
`)
	// tools/ subtree must be skipped.
	writeFile(t, dir, "modules/tools/x.go", `package tools
func z(c *Ctx) { c.AbortWithStatusJSON(400, nil) }
`)

	counts, err := countSites([]string{filepath.Join(dir, "modules")})
	if err != nil {
		t.Fatalf("countSites: %v", err)
	}

	aPath := filepath.ToSlash(filepath.Join(dir, "modules/a/api.go"))
	if counts[aPath] != 2 {
		t.Fatalf("api.go count = %d, want 2 (AbortWithStatusJSON + AbortWithStatus, not c.JSON)", counts[aPath])
	}
	if len(counts) != 1 {
		t.Fatalf("expected only api.go in counts, got %d entries: %#v", len(counts), counts)
	}
}

func TestLoadBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	writeFile(t, dir, "baseline.txt", `# comment
8 modules/user/api.go

37 modules/oidc/api_bind.go
`)
	b, err := loadBaseline(path)
	if err != nil {
		t.Fatalf("loadBaseline: %v", err)
	}
	if b["modules/user/api.go"] != 8 {
		t.Fatalf("user baseline = %d, want 8", b["modules/user/api.go"])
	}
	if b["modules/oidc/api_bind.go"] != 37 {
		t.Fatalf("oidc baseline = %d, want 37", b["modules/oidc/api_bind.go"])
	}
	if len(b) != 2 {
		t.Fatalf("baseline entries = %d, want 2 (comment + blank ignored)", len(b))
	}
}

func TestLoadBaseline_TrailingAnnotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	// PR #193 review: rows may carry a trailing EXEMPT note / inline comment.
	writeFile(t, dir, "baseline.txt", `4 modules/webhook/github.go  # EXEMPT: external GH
9 modules/app_bot/app_bot.go EXEMPT
8 modules/user/api.go
`)
	b, err := loadBaseline(path)
	if err != nil {
		t.Fatalf("loadBaseline with annotations: %v", err)
	}
	if b["modules/webhook/github.go"] != 4 {
		t.Fatalf("inline-comment row = %d, want 4", b["modules/webhook/github.go"])
	}
	if b["modules/app_bot/app_bot.go"] != 9 {
		t.Fatalf("trailing-token row = %d, want 9", b["modules/app_bot/app_bot.go"])
	}
	if b["modules/user/api.go"] != 8 {
		t.Fatalf("plain row = %d, want 8", b["modules/user/api.go"])
	}
}

func TestLoadBaseline_Malformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.txt")
	writeFile(t, dir, "baseline.txt", "notanumber modules/x.go\n")
	if _, err := loadBaseline(path); err == nil {
		t.Fatal("expected error on malformed count, got nil")
	}
}

// classify mirrors the main() decision logic so the regression / advisory
// split can be unit-tested without spawning a subprocess.
func classify(counts, baseline map[string]int) (regressions, advisories int) {
	for file, n := range counts {
		if n > baseline[file] {
			regressions++
		}
	}
	for file, allowed := range baseline {
		if counts[file] < allowed {
			advisories++
		}
	}
	return
}

func TestRatchetDecisions(t *testing.T) {
	baseline := map[string]int{
		"modules/user/api.go": 8,
		"modules/oidc/api.go": 14,
	}

	t.Run("equal counts pass", func(t *testing.T) {
		r, a := classify(map[string]int{"modules/user/api.go": 8, "modules/oidc/api.go": 14}, baseline)
		if r != 0 || a != 0 {
			t.Fatalf("regressions=%d advisories=%d, want 0/0", r, a)
		}
	})

	t.Run("increase is a regression", func(t *testing.T) {
		r, _ := classify(map[string]int{"modules/user/api.go": 9}, baseline)
		if r != 1 {
			t.Fatalf("regressions=%d, want 1", r)
		}
	})

	t.Run("brand-new file is a regression", func(t *testing.T) {
		r, _ := classify(map[string]int{"modules/new/api.go": 1}, baseline)
		if r != 1 {
			t.Fatalf("regressions=%d, want 1 (new file off baseline)", r)
		}
	})

	t.Run("decrease is an advisory not a regression", func(t *testing.T) {
		r, a := classify(map[string]int{"modules/user/api.go": 0, "modules/oidc/api.go": 14}, baseline)
		if r != 0 {
			t.Fatalf("regressions=%d, want 0 (migration reduced count)", r)
		}
		if a != 1 {
			t.Fatalf("advisories=%d, want 1 (baseline should tighten)", a)
		}
	})
}
