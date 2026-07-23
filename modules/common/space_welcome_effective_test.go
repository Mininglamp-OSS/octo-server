package common

import "testing"

// TestResolveEffectiveSpaceWelcome pins the precedence contract between a
// per-Space config row and the platform-global fallback (task
// space-welcome-per-space-admin-crud). Pure function — no DB.
func TestResolveEffectiveSpaceWelcome(t *testing.T) {
	globalOn := SpaceWelcomeConfig{
		Enabled:       true,
		SpaceID:       "spc_global",
		ActiveFromRaw: "2026-07-01T00:00:00Z",
		Message:       "global hi",
	}
	globalOff := SpaceWelcomeConfig{Enabled: false, SpaceID: "spc_global"}

	row := func(enabled bool, msg string) *SpaceWelcomeSpaceConfig {
		return &SpaceWelcomeSpaceConfig{
			SpaceID:       "spc_x",
			Enabled:       enabled,
			ActiveFromRaw: "2026-07-10T00:00:00Z",
			Message:       msg,
		}
	}

	cases := []struct {
		name        string
		row         *SpaceWelcomeSpaceConfig
		found       bool
		global      SpaceWelcomeConfig
		spaceID     string
		wantEnabled bool
		wantMessage string
	}{
		{
			name: "per-space enabled row wins over global",
			row:  row(true, "space hi"), found: true, global: globalOn, spaceID: "spc_x",
			wantEnabled: true, wantMessage: "space hi",
		},
		{
			name: "per-space DISABLED row overrides global that names this space",
			// Even when the global config names spc_x and is on, a present row
			// (disabled) opts the space OUT — a present row always wins.
			row:         &SpaceWelcomeSpaceConfig{SpaceID: "spc_x", Enabled: false},
			found:       true,
			global:      SpaceWelcomeConfig{Enabled: true, SpaceID: "spc_x", ActiveFromRaw: "2026-07-01T00:00:00Z", Message: "global"},
			spaceID:     "spc_x",
			wantEnabled: false, wantMessage: "",
		},
		{
			name: "no row, global enabled and names this space -> global applies",
			row:  nil, found: false, global: globalOn, spaceID: "spc_global",
			wantEnabled: true, wantMessage: "global hi",
		},
		{
			name: "no row, global enabled but names a DIFFERENT space -> off",
			row:  nil, found: false, global: globalOn, spaceID: "spc_other",
			wantEnabled: false, wantMessage: "",
		},
		{
			name: "no row, global disabled -> off",
			row:  nil, found: false, global: globalOff, spaceID: "spc_global",
			wantEnabled: false, wantMessage: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveEffectiveSpaceWelcome(tc.row, tc.found, tc.global, tc.spaceID)
			if got.Enabled != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", got.Enabled, tc.wantEnabled)
			}
			if got.Message != tc.wantMessage {
				t.Fatalf("Message = %q, want %q", got.Message, tc.wantMessage)
			}
			if got.SpaceID != tc.spaceID {
				t.Fatalf("SpaceID = %q, want %q (effective config always carries the queried space)", got.SpaceID, tc.spaceID)
			}
		})
	}
}
