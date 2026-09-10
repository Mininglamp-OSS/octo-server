package errcode

import (
	"net/http"
	"testing"
)

func TestManagerMFACodesExposeStableHTTPAndInternalSemantics(t *testing.T) {
	// Keep the two assertions below explicit because these codes are used by
	// unauthenticated handlers and a renderer regression could turn a 503 into
	// a client-visible internal detail.
	for _, code := range []struct {
		name     string
		status   int
		internal bool
	}{
		{"settings unavailable", ErrUserManagerMFASettingsUnavailable.HTTPStatus, ErrUserManagerMFASettingsUnavailable.Internal},
		{"misconfigured", ErrUserManagerMFAMisconfigured.HTTPStatus, ErrUserManagerMFAMisconfigured.Internal},
	} {
		if code.status != http.StatusServiceUnavailable {
			t.Errorf("%s HTTPStatus = %d, want 503", code.name, code.status)
		}
		if !code.internal {
			t.Errorf("%s must be Internal=true", code.name)
		}
	}
}
