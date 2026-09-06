package errcode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// TestProjectCodesInternalFlag mirrors the shared-code invariant for
// err.server.project.*: only 5xx codes may be Internal=true, and every 5xx code MUST
// be — otherwise the renderer leaks a raw storage message on a server error (D11/D13).
//
// Written as a loop over the registry rather than over a hand-listed slice on purpose:
// a code added later is covered without anyone remembering to extend this test, which is
// the failure mode a hand-listed version has.
func TestProjectCodesInternalFlag(t *testing.T) {
	seen := 0
	for _, c := range codes.All() {
		if !strings.HasPrefix(c.ID, "err.server.project.") {
			continue
		}
		seen++
		is5xx := c.HTTPStatus >= 500 && c.HTTPStatus < 600
		if is5xx && !c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=false; 5xx must be Internal=true", c.ID, c.HTTPStatus)
		}
		if !is5xx && c.Internal {
			t.Errorf("%s: HTTPStatus=%d but Internal=true; only 5xx may be Internal", c.ID, c.HTTPStatus)
		}
	}
	if seen == 0 {
		t.Fatal("no err.server.project.* codes registered; this guard would pass vacuously")
	}
}

// TestProjectNotFoundIsGenericAndDetailFree pins the anti-enumeration contract.
//
// ErrProjectNotFound is the single answer for three different situations: the project
// does not exist, it exists in a Space the caller is not in, and it exists but is
// unlisted and the caller is not a member. The bodies must be indistinguishable, so the
// code carries NO SafeDetailKeys — any whitelisted detail key would let a caller tell
// the three apart, which is exactly the existence oracle this merge prevents.
func TestProjectNotFoundIsGenericAndDetailFree(t *testing.T) {
	if ErrProjectNotFound.HTTPStatus != http.StatusNotFound {
		t.Fatalf("not_found status = %d, want 404", ErrProjectNotFound.HTTPStatus)
	}
	if len(ErrProjectNotFound.SafeDetailKeys) != 0 {
		t.Errorf("not_found must carry no SafeDetailKeys (anti-enumeration), got %v",
			ErrProjectNotFound.SafeDetailKeys)
	}
	// The message must not hint at which of the three reasons applied.
	msg := strings.ToLower(ErrProjectNotFound.DefaultMessage)
	for _, leak := range []string{"space", "member", "unlisted", "permission", "forbidden"} {
		if strings.Contains(msg, leak) {
			t.Errorf("not_found message %q hints at the reason via %q", ErrProjectNotFound.DefaultMessage, leak)
		}
	}
}

// TestProjectQuotaCodesSurfaceTheirLimit pins that each quota code can tell the client
// what the limit was. The acceptance criterion is that limits come from config rather
// than literals; a client that cannot read the limit ends up hard-coding it anyway,
// which reintroduces the literal on the other side of the wire.
func TestProjectQuotaCodesSurfaceTheirLimit(t *testing.T) {
	for _, c := range []codes.Code{
		ErrProjectQuotaPerSpace,
		ErrProjectQuotaPerCreator,
		ErrProjectQuotaMembers,
		ErrProjectQuotaDailyCreate,
	} {
		registered, ok := codes.Lookup(c.ID)
		if !ok {
			t.Errorf("%s not registered", c.ID)
			continue
		}
		found := false
		for _, k := range registered.SafeDetailKeys {
			if k == "max" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must whitelist the \"max\" detail so the client can render the limit", c.ID)
		}
	}
}
