package bot_task

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBotTaskNoLegacyResponseError(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob module files: %v", err)
	}
	banned := []string{".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus("}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`c\.JSON\(\s*(?:http\.Status[A-Z]|[1-5]\d{2}\b)`),
		regexp.MustCompile(`c\.AbortWithStatus(?:JSON)?\(`),
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		cleaned := string(data)
		for _, token := range banned {
			if strings.Contains(cleaned, token) {
				t.Fatalf("%s must use localized errors, found %s", file, token)
			}
		}
		for _, pattern := range patterns {
			if match := pattern.FindString(cleaned); match != "" {
				t.Fatalf("%s contains raw error response %q", file, match)
			}
		}
	}
}
