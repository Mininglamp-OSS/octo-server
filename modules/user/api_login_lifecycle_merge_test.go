package user

import "testing"

// TestLoginLifecycleHelpersRemainIntegrated guards the merge boundary between
// the v3 APP-session lifecycle and login-audit finalization. Both helpers are
// load-bearing and must coexist even when adjacent changes land on main.
func TestLoginLifecycleHelpersRemainIntegrated(t *testing.T) {
	_ = (*User).replaceAPPTokenSession
	_ = (*User).finishSuccessfulLogin
}
