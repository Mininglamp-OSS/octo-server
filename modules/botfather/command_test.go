package botfather

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

func newTestCommandHandler() *commandHandler {
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	return newCommandHandler(ctx)
}

func TestHandleCommand_EmptyParts(t *testing.T) {
	h := newTestCommandHandler()

	// These should not panic - the fix adds bounds checking
	tests := []struct {
		name string
		cmd  string
	}{
		{"just slash", "/"},
		{"slash with spaces", "/   "},
		{"empty string", ""},
		{"only spaces", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic
			assert.NotPanics(t, func() {
				h.handleCommand("test_user", tt.cmd)
			})
		})
	}
}

func TestHandleCommand_ValidCommands(t *testing.T) {
	h := newTestCommandHandler()

	// Valid commands should not panic
	validCmds := []string{
		"/help",
		"/start",
		"/newbot",
		"/mybots",
		"/cancel",
		"/help extra args",
		"/HELP",           // uppercase
		"/help   spaces",  // extra spaces
	}

	for _, cmd := range validCmds {
		t.Run(cmd, func(t *testing.T) {
			assert.NotPanics(t, func() {
				h.handleCommand("test_user", cmd)
			})
		})
	}
}

func TestHandleCommand_UnknownCommand(t *testing.T) {
	h := newTestCommandHandler()

	// Unknown commands should not panic
	assert.NotPanics(t, func() {
		h.handleCommand("test_user", "/unknowncmd")
	})
}

func TestHandleMessage_EmptyContent(t *testing.T) {
	h := newTestCommandHandler()

	// Empty content should be handled gracefully
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"spaces only", "   "},
		{"tabs", "\t\t"},
		{"newlines", "\n\n"},
		{"mixed whitespace", "  \t\n  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NotPanics(t, func() {
				h.HandleMessage("test_user", tt.content)
			})
		})
	}
}

func TestHandleMessage_SlashOnly(t *testing.T) {
	h := newTestCommandHandler()

	// Single slash should not panic
	assert.NotPanics(t, func() {
		h.HandleMessage("test_user", "/")
	})
}
