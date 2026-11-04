package errcode

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

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
