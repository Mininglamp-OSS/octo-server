package notification

import (
	"os"
	"strings"
	"testing"
)

// TestNotificationNoLegacyResponseError keeps notification handlers on the
// localized response envelope and prevents a future raw error response from
// bypassing the registered notification error codes.
func TestNotificationNoLegacyResponseError(t *testing.T) {
	for _, file := range []string{"api.go", "api_i18n.go"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		cleaned := strings.Builder{}
		for _, line := range strings.Split(string(data), "\n") {
			if index := strings.Index(line, "//"); index >= 0 {
				line = line[:index]
			}
			cleaned.WriteString(line)
			cleaned.WriteByte('\n')
		}
		for _, banned := range []string{
			".ResponseError(", ".ResponseErrorf(", ".ResponseErrorWithStatus(",
			".AbortWithStatusJSON(", ".AbortWithStatus(", "c.Response(\"",
		} {
			if strings.Contains(cleaned.String(), banned) {
				t.Fatalf("modules/notification/%s must use httperr.ResponseErrorL* instead of legacy %s", file, banned)
			}
		}
	}
}
