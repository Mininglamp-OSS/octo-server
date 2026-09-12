package internal_membership

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleGoFiles lists this module's production sources.
//
// Globbed, not hand-listed. Every guard below then covers a file the moment it is
// added, which is the whole difference between a guard and a snapshot: a
// hand-maintained list silently stops covering the file someone forgot to add,
// and the forgotten file is exactly the one nobody reviewed twice.
func moduleGoFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob module files: %v", err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no production files found; every guard in this file would pass vacuously")
	}
	return out
}

func strippedSource(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var clean strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	return clean.String()
}

// TestInternalMembershipNoLegacyResponseError pins the module to the localized
// error envelope. Mirrors modules/internal_resolve's guard verbatim, including
// the glob, so a handler file added later is covered without anyone remembering
// to extend a list.
func TestInternalMembershipNoLegacyResponseError(t *testing.T) {
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`c\.JSON\(\s*(?:http\.Status[A-Z]|[1-5]\d{2}\b)`),
		regexp.MustCompile(`c\.AbortWithStatus(?:JSON)?\(`),
	}
	for _, file := range moduleGoFiles(t) {
		t.Run(file, func(t *testing.T) {
			cleaned := strippedSource(t, file)
			for _, token := range banned {
				if strings.Contains(cleaned, token) {
					t.Fatalf("modules/internal_membership/%s must use httperr.ResponseErrorLWithStatus, found %s", file, token)
				}
			}
			for _, pattern := range patterns {
				if match := pattern.FindString(cleaned); match != "" {
					t.Fatalf("modules/internal_membership/%s contains banned raw error response %q", file, match)
				}
			}
		})
	}
}

// TestTokenCompareStaysConstantTime pins the one line that makes the token check
// safe to expose to an unauthenticated caller.
//
// Mutating subtle.ConstantTimeCompare to a plain `!=` survives every behavioural
// test in this module: the two are functionally identical, and the difference is
// only observable as timing. The naive replacement happens to break the build by
// orphaning the import, but `bytes.Equal` or a helper would not — and a timing
// oracle on an endpoint whose strict bucket is 20 rps is a token recovered in a
// bounded number of requests.
func TestTokenCompareStaysConstantTime(t *testing.T) {
	cleaned := strippedSource(t, "api.go")
	start := strings.Index(cleaned, "func (m *Module) internalAuthMiddleware(")
	if start < 0 {
		t.Fatal("internalAuthMiddleware not found — if it moved, point this guard at it rather than deleting it")
	}
	body := cleaned[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, "subtle.ConstantTimeCompare(") {
		t.Error("the token compare must be subtle.ConstantTimeCompare. A byte-wise comparison " +
			"returns early on the first differing byte, which leaks the shared secret one " +
			"byte at a time to any caller who can time the 401")
	}
	for _, banned := range []string{"bytes.Equal(", "token == m.internalToken", "token != m.internalToken"} {
		if strings.Contains(body, banned) {
			t.Errorf("internalAuthMiddleware must not compare the token with %s — same early return, same leak", banned)
		}
	}
}
