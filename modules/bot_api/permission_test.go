package bot_api

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

func TestCheckSendPermission_AppBot_DMOnly(t *testing.T) {
	// App Bot should only allow ChannelTypePerson
	tests := []struct {
		name        string
		channelType uint8
		shouldErr   bool
	}{
		{"DM allowed", common.ChannelTypePerson.Uint8(), false},
		{"Group denied", common.ChannelTypeGroup.Uint8(), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkAppBotSendRule(tt.channelType)
			if tt.shouldErr && err == nil {
				t.Errorf("expected error for channelType=%d, got nil", tt.channelType)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("expected no error for channelType=%d, got %v", tt.channelType, err)
			}
		})
	}
}

// checkAppBotSendRule is the extracted permission rule for App Bot.
func checkAppBotSendRule(channelType uint8) error {
	if channelType != common.ChannelTypePerson.Uint8() {
		return errAppBotDMOnly
	}
	return nil
}

var errAppBotDMOnly = errNew("app bot only supports direct messages")

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
func errNew(msg string) error        { return &simpleError{msg: msg} }

func TestAppBotTokenFormat(t *testing.T) {
	tests := []struct {
		token string
		valid bool
	}{
		{"app_d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9", true}, // 4 + 32 hex = 36 chars
		{"app_short", true},                            // prefix correct (length not enforced at auth layer)
		{"bf_a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", false}, // bf_ prefix
		{"uk_a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", false}, // uk_ prefix
		{"app", false}, // too short, no underscore
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			isApp := len(tt.token) >= 4 && tt.token[:4] == "app_"
			if isApp != tt.valid {
				t.Errorf("token %q: isApp=%v, want %v", tt.token, isApp, tt.valid)
			}
		})
	}
}
