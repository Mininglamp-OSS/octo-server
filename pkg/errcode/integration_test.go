package errcode

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

func TestIntegrationCodesRegistered(t *testing.T) {
	for _, tc := range []struct {
		code   codes.Code
		id     string
		status int
	}{
		{ErrIntegrationDisabled, "err.server.integration.disabled", http.StatusForbidden},
		{ErrIntegrationUserNotLinked, "err.server.integration.user_not_linked", http.StatusForbidden},
		{ErrBotOccupied, "err.server.bot.occupied", http.StatusConflict},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if tc.code.ID != tc.id {
				t.Fatalf("var ID = %q, want %q", tc.code.ID, tc.id)
			}
			got, ok := codes.Lookup(tc.id)
			if !ok {
				t.Fatalf("%s not registered", tc.id)
			}
			if got.HTTPStatus != tc.status {
				t.Fatalf("%s HTTPStatus = %d, want %d", tc.id, got.HTTPStatus, tc.status)
			}
			if got.DefaultMessage == "" {
				t.Fatalf("%s missing en DefaultMessage", tc.id)
			}
		})
	}
}

func TestBotOccupiedWhitelistsOccupiedBy(t *testing.T) {
	got, ok := codes.Lookup("err.server.bot.occupied")
	if !ok {
		t.Fatal("err.server.bot.occupied not registered")
	}
	found := false
	for _, k := range got.SafeDetailKeys {
		if k == "occupied_by" {
			found = true
		}
	}
	if !found {
		t.Fatalf("err.server.bot.occupied must whitelist occupied_by, got %v", got.SafeDetailKeys)
	}
}
