package user

import "testing"

// TestManagerCapabilities pins the /v1/manager/me capability map: superAdmin-only
// features must be false for a plain admin, while admin∪superAdmin features are
// true for both. Pure function — no server / DB needed.
func TestManagerCapabilities(t *testing.T) {
	super := managerCapabilities(true)
	admin := managerCapabilities(false)

	superOnly := []string{
		"system_setting", "backup", "appversion.write", "dashboard.trigger", "space.destructive",
	}
	adminTier := []string{
		"appversion.read", "dashboard.read", "users", "groups", "space.read",
	}

	for _, k := range superOnly {
		if !super[k] {
			t.Errorf("superAdmin must have capability %q", k)
		}
		if admin[k] {
			t.Errorf("admin must NOT have superAdmin-only capability %q", k)
		}
	}
	for _, k := range adminTier {
		if !super[k] || !admin[k] {
			t.Errorf("admin-tier capability %q must be true for both admin and superAdmin", k)
		}
	}

	// Guard against a key being silently dropped/renamed out of the contract.
	if got, want := len(super), len(superOnly)+len(adminTier); got != want {
		t.Errorf("capability map has %d keys, want %d (update this test if the contract changed)", got, want)
	}
}
