package space

import (
	"testing"
	"time"

	commonmod "github.com/Mininglamp-OSS/octo-server/modules/common"
)

// TestWelcomeSource pins the source label shown to admins (task
// space-welcome-per-space-admin-crud). Pure — no DB.
func TestWelcomeSource(t *testing.T) {
	cases := []struct {
		name  string
		found bool
		eff   commonmod.SpaceWelcomeConfig
		want  string
	}{
		{"present row is always source=space (even disabled)", true, commonmod.SpaceWelcomeConfig{Enabled: false}, "space"},
		{"present enabled row is source=space", true, commonmod.SpaceWelcomeConfig{Enabled: true}, "space"},
		{"no row, global in effect is source=global", false, commonmod.SpaceWelcomeConfig{Enabled: true}, "global"},
		{"no row, nothing in effect is source=none", false, commonmod.SpaceWelcomeConfig{Enabled: false}, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := welcomeSource(tc.found, tc.eff); got != tc.want {
				t.Fatalf("welcomeSource(%v, enabled=%v) = %q, want %q", tc.found, tc.eff.Enabled, got, tc.want)
			}
		})
	}
}

// TestWelcomeClientField maps the shared validator's key to the PUT field name.
func TestWelcomeClientField(t *testing.T) {
	cases := map[string]string{
		"space_welcome_active_from": "active_from",
		"space_welcome_message":     "message",
		"space_welcome_space_id":    "space_id",
		"unknown_key":               "unknown_key",
	}
	for in, want := range cases {
		if got := welcomeClientField(in); got != want {
			t.Fatalf("welcomeClientField(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildWelcomeResp checks the response assembly for both the configured and
// fallback shapes.
func TestBuildWelcomeResp(t *testing.T) {
	t.Run("configured row surfaces its fields + source=space", func(t *testing.T) {
		row := &commonmod.SpaceWelcomeSpaceConfig{
			SpaceID:       "spc_1",
			Enabled:       true,
			ActiveFromRaw: "2026-07-10T00:00:00Z",
			Message:       "hello\nworld",
			UpdatedBy:     "u_admin",
			UpdatedAt:     time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC),
		}
		eff := commonmod.SpaceWelcomeConfig{Enabled: true, SpaceID: "spc_1", Message: "hello\nworld"}
		resp := buildWelcomeResp(row, true, eff)
		if !resp.Configured || !resp.Enabled {
			t.Fatalf("Configured/Enabled = %v/%v, want true/true", resp.Configured, resp.Enabled)
		}
		if resp.Message != "hello\nworld" {
			t.Fatalf("Message = %q, want newline preserved verbatim", resp.Message)
		}
		if resp.UpdatedBy != "u_admin" || resp.UpdatedAt != "2026-07-10T01:02:03Z" {
			t.Fatalf("audit fields not surfaced/formatted (want RFC3339 updated_at): %+v", resp)
		}
		if resp.Effective.Source != "space" || !resp.Effective.Enabled {
			t.Fatalf("effective = %+v, want source=space enabled=true", resp.Effective)
		}
	})

	t.Run("no row, global fallback in effect -> not configured, source=global", func(t *testing.T) {
		eff := commonmod.SpaceWelcomeConfig{Enabled: true, SpaceID: "spc_1", Message: "global"}
		resp := buildWelcomeResp(nil, false, eff)
		if resp.Configured {
			t.Fatalf("Configured = true, want false (no per-space row)")
		}
		if resp.Effective.Source != "global" || resp.Effective.Message != "global" {
			t.Fatalf("effective = %+v, want source=global message=global", resp.Effective)
		}
	})
}
