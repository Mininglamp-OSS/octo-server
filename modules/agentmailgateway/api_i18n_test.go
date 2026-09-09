package agentmailgateway

import (
	"os"
	"strings"
	"testing"
)

// TestAgentMailGatewayNoLegacyErrorResponse keeps locally generated errors on
// the i18n envelope. The transparent proxy deliberately writes the upstream
// status and body through c.Status/c.Writer, so those success/error passthrough
// primitives are not banned here.
func TestAgentMailGatewayNoLegacyErrorResponse(t *testing.T) {
	data, err := os.ReadFile("gateway.go")
	if err != nil {
		t.Fatal(err)
	}
	var clean strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	for _, banned := range []string{
		".ResponseError(",
		".ResponseErrorf(",
		".ResponseErrorWithStatus(",
		".AbortWithStatusJSON(",
		".AbortWithStatus(",
		"c.JSON(",
	} {
		if strings.Contains(clean.String(), banned) {
			t.Fatalf("gateway.go must render local errors through respondGatewayError instead of %s", banned)
		}
	}
}
