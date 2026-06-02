package errcode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// TestOIDCCodesRegistered asserts every err.server.oidc.* code this package
// declares is actually registered (a typo'd ID would otherwise only surface as a
// runtime fallback to err.shared.internal).
func TestOIDCCodesRegistered(t *testing.T) {
	want := []codes.Code{
		ErrOIDCBindServiceUnavailable,
		ErrOIDCBindRequestInvalid,
		ErrOIDCBindSMSUnavailable,
		ErrOIDCBindMethodDisabled,
		ErrOIDCBindInvalidCredentials,
		ErrOIDCBindVerifyRequired,
		ErrOIDCBindAlreadyVerified,
		ErrOIDCBindStatusConflict,
		ErrOIDCBindAlreadyBound,
		ErrOIDCBindConflictNeedManual,
		ErrOIDCBindTokenInvalid,
		ErrOIDCBindClaimsIncomplete,
	}
	for _, c := range want {
		if _, ok := codes.Lookup(c.ID); !ok {
			t.Errorf("code %q not registered", c.ID)
		}
	}
}

// TestOIDCCodesInternalFlag mirrors the shared-code invariant for err.server.oidc.*:
// only 5xx codes may be Internal=true, and every 5xx code MUST be — otherwise the
// renderer would leak a raw message on a server error (D11/D13).
func TestOIDCCodesInternalFlag(t *testing.T) {
	for _, c := range codes.All() {
		if !strings.HasPrefix(c.ID, "err.server.oidc.") {
			continue
		}
		is5xx := c.HTTPStatus >= 500 && c.HTTPStatus < 600
		if is5xx && !c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=false; 5xx must be Internal=true", c.ID, c.HTTPStatus)
		}
		if !is5xx && c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=true; only 5xx may be Internal", c.ID, c.HTTPStatus)
		}
	}
}

// TestOIDCInvalidCredentialsGeneric guards the anti-enumeration contract: the
// 401 bind code must not hint at which factor failed.
func TestOIDCInvalidCredentialsGeneric(t *testing.T) {
	if ErrOIDCBindInvalidCredentials.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("invalid_credentials status = %d, want 401", ErrOIDCBindInvalidCredentials.HTTPStatus)
	}
	msg := strings.ToLower(ErrOIDCBindInvalidCredentials.DefaultMessage)
	for _, leak := range []string{"password", "account not found", "user not found", "phone", "otp"} {
		if strings.Contains(msg, leak) {
			t.Errorf("invalid_credentials message leaks enumeration hint %q: %q", leak, ErrOIDCBindInvalidCredentials.DefaultMessage)
		}
	}
}
