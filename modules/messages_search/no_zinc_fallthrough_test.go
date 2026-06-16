package messages_search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// V5 — the es read path must NEVER fall through to Zinc/WuKongIM search. We
// enforce this structurally: no non-test source file in modules/messages_search
// may invoke the legacy Zinc query surface (the WuKongIM /message/search
// forward, the IMSearchUserMessages global search, or modules/search). An es
// deployment whose OpenSearch is down surfaces UPSTREAM_UNAVAILABLE (see
// classifyOSError + respondUpstream) instead of silently returning Zinc
// results. Reading the ZincSearch.SearchOn config toggle to compute the
// default backend is a declaration read (allowed), not a query fall-through.
func TestNoZincFallthrough(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	banned := []string{
		"IMSearchUserMessages",
		"/message/search",
		"modules/search",
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// Strip // line comments so doc cross-references (e.g. "mirrors
		// modules/search/api.go's predicate") don't trip the structural ban —
		// we only care about live code invoking the legacy surface. Same
		// comment-stripping approach as api_i18n_test.go.
		var clean strings.Builder
		for _, line := range strings.Split(string(data), "\n") {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			clean.WriteString(line)
			clean.WriteByte('\n')
		}
		cleaned := clean.String()
		for _, b := range banned {
			if strings.Contains(cleaned, b) {
				t.Fatalf("%s references the legacy Zinc surface %q — the es read path "+
					"must never fall through to Zinc (V5)", name, b)
			}
		}
	}
}
