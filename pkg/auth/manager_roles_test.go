package auth

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

func TestManagerRolePolicy(t *testing.T) {
	tests := []struct {
		name              string
		role              string
		wantConsole       bool
		wantDashboardRead bool
		wantMarketplace   bool
	}{
		{name: "super admin", role: string(wkhttp.SuperAdmin), wantConsole: true, wantDashboardRead: true, wantMarketplace: true},
		{name: "admin", role: string(wkhttp.Admin), wantConsole: true, wantDashboardRead: true},
		{name: "dashboard reader", role: ManagerRoleDashboardReader, wantConsole: true, wantDashboardRead: true},
		// marketAdmin may enter the console, but holds neither dashboard read nor
		// any admin-tier power — its only privilege is the platform catalog surface.
		{name: "market admin", role: ManagerRoleMarketAdmin, wantConsole: true, wantMarketplace: true},
		{name: "plain user", role: "", wantConsole: false, wantDashboardRead: false},
		{name: "unknown role", role: "unknown", wantConsole: false, wantDashboardRead: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsManagerConsoleRole(tt.role); got != tt.wantConsole {
				t.Fatalf("IsManagerConsoleRole(%q) = %v, want %v", tt.role, got, tt.wantConsole)
			}
			if got := CanReadManagerDashboard(tt.role); got != tt.wantDashboardRead {
				t.Fatalf("CanReadManagerDashboard(%q) = %v, want %v", tt.role, got, tt.wantDashboardRead)
			}
			if got := CanAdminMarketplace(tt.role); got != tt.wantMarketplace {
				t.Fatalf("CanAdminMarketplace(%q) = %v, want %v", tt.role, got, tt.wantMarketplace)
			}
		})
	}
}
