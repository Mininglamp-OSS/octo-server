package user

import (
	"os"
	"strings"
	"testing"
)

// Email registration and reset must validate the exact password bytes that are
// later hashed. Trimming only for validation can let an over-72-byte raw value
// pass policy and fail later as a generic bcrypt processing error.
func TestEmailPasswordPolicyValidatesStoredBytes(t *testing.T) {
	source, err := os.ReadFile("api_emaillogin.go")
	if err != nil {
		t.Fatalf("read api_emaillogin.go: %v", err)
	}

	text := string(source)
	if strings.Contains(text, "ValidatePasswordStrength(strings.TrimSpace(req.Password))") {
		t.Fatal("email password policy validates a trimmed value but hashes the raw value")
	}
	if got := strings.Count(text, "ValidatePasswordStrength(req.Password)"); got < 2 {
		t.Fatalf("raw password validation wired %d times, want at least registration and reset", got)
	}
}
