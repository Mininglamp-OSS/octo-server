package errcode

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// TestDrivePolicyCodesInternalFlag mirrors the shared-code invariant for
// err.server.drive_policy.*: only 5xx codes may be Internal=true, and every 5xx
// code MUST be — otherwise the renderer would leak a raw message on a server
// error (D11/D13).
//
// Written as a loop over the registry (like TestProjectCodesInternalFlag) so a
// code added later is covered without anyone remembering to extend this test.
//
// Purpose in this workflow: it is the guard that keeps the fail-closed 503 on the
// shared err.shared.internal copy. A future edit that drops Internal=true to
// surface the bespoke "not available yet" message would trip here (and in
// modules/drivepolicy) instead of silently making 5xx bodies vary.
func TestDrivePolicyCodesInternalFlag(t *testing.T) {
	seen := 0
	for _, c := range codes.All() {
		if !strings.HasPrefix(c.ID, "err.server.drive_policy.") {
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
		t.Fatal("no err.server.drive_policy.* codes registered; this guard would pass vacuously")
	}
}
