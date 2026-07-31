package bot_mention

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestBotMentionNoLegacyResponseError pins the new internal endpoint to the
// localized error envelope. Add future handler files to this list.
func TestBotMentionNoLegacyResponseError(t *testing.T) {
	files := []string{"api.go", "api_i18n.go"}
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
