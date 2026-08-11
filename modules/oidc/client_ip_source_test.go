package oidc

import (
	"os"
	"strings"
	"testing"
)

func TestSecuritySensitivePathsUseWKHTTPClientIP(t *testing.T) {
	tests := []struct {
		file      string
		wantCalls int
	}{
		{file: "api.go", wantCalls: 3},
		{file: "api_bind.go", wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile(tt.file)
			if err != nil {
				t.Fatalf("read %s: %v", tt.file, err)
			}
			source := string(data)
			if strings.Contains(source, "util.GetClientPublicIP(c.Request)") {
				t.Fatalf("%s still uses spoofable util.GetClientPublicIP", tt.file)
			}
			if got := strings.Count(source, "wkhttp.ClientIP(c.Request)"); got != tt.wantCalls {
				t.Fatalf("%s wkhttp.ClientIP calls = %d, want %d", tt.file, got, tt.wantCalls)
			}
		})
	}
}
