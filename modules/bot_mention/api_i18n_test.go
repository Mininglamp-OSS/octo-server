package bot_mention

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBotMentionNoLegacyResponseError pins the module to the localized error
// envelope and automatically covers future production Go files.
func TestBotMentionNoLegacyResponseError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob module files: %v", err)
	}
	banned := []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`c\.JSON\(\s*(?:http\.Status[A-Z]|[1-5]\d{2}\b)`),
		regexp.MustCompile(`c\.AbortWithStatus(?:JSON)?\(`),
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		t.Run(file, func(t *testing.T) {
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
			cleaned := clean.String()
			for _, token := range banned {
				if strings.Contains(cleaned, token) {
					t.Fatalf("modules/bot_mention/%s must use httperr.ResponseErrorLWithStatus, found %s", file, token)
				}
			}
			for _, pattern := range patterns {
				if match := pattern.FindString(cleaned); match != "" {
					t.Fatalf("modules/bot_mention/%s contains banned raw error response %q", file, match)
				}
			}
		})
	}
}
