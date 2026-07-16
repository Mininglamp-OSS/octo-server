package messages_search

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMessagesSearchNoLegacyResponseError pins the contract that this module's
// handlers go through pkg/httperr.ResponseErrorL helpers (respond* in
// api_i18n.go) rather than the legacy octo-lib c.ResponseError / c.ResponseErrorf APIs or
// raw c.JSON / c.AbortWithStatus* calls.
func TestMessagesSearchNoLegacyResponseError(t *testing.T) {
	files := []string{
		"api.go",
		"authz.go",
		"search_messages.go",
		"search_media.go",
		"search_files.go",
		"search_all.go",
		"api_i18n.go",
		"validate.go",
		"ratelimit.go",
		"audit.go",
		"space_scope.go",
		"visibility.go",
		"principal.go",
		"predicate.go",
	}
	// Banned literals — exact substring match.
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
	}
	// Banned patterns — covers every status code (4xx/5xx, http.StatusXXX
	// or numeric literal) and the Abort variants that write a response
	// body. Bare c.Abort() is allowed: it only stops the gin chain after
	// a respond* helper has already produced the envelope.
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`c\.JSON\(\s*(?:http\.Status[A-Z]|[1-5]\d{2}\b)`),
		regexp.MustCompile(`c\.AbortWithStatus(?:JSON)?\(`),
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
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
					t.Fatalf("modules/messages_search/%s must use httperr.ResponseErrorL via respond* helpers, not %s", f, b)
				}
			}
			for _, re := range patterns {
				if loc := re.FindStringIndex(cleaned); loc != nil {
					t.Fatalf("modules/messages_search/%s must use httperr.ResponseErrorL via respond* helpers, banned pattern matched: %q",
						f, cleaned[loc[0]:loc[1]])
				}
			}
		})
	}
}
