package user

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAllowedUpdateFieldsWhitelist(t *testing.T) {
	// Test that the whitelist contains expected fields
	expectedAllowed := []string{
		"sex", "short_no", "name", "short_status",
		"search_by_phone", "search_by_short", "new_msg_notice",
		"msg_show_detail", "voice_on", "shock_on",
		"msg_expire_second", "is_upload_avatar",
		"chat_pwd", "lock_after_minute", "lock_screen_pwd",
	}

	for _, field := range expectedAllowed {
		assert.True(t, allowedUpdateFields[field], "field %s should be allowed", field)
	}
}

func TestAllowedUpdateFieldsBlocked(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	// Test that dangerous fields are not allowed
	blockedFields := []string{
		"uid", "id", "password", "role", "admin",
		"created_at", "updated_at", "phone", "email",
	}

	for _, field := range blockedFields {
		assert.False(t, allowedUpdateFields[field], "field %s should be blocked", field)
	}
}

func TestUpdateUsersWithFieldBlockedFields(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	db := &DB{session: nil} // session is nil but we're testing validation only

	blockedFields := []string{
		"password", "uid", "role", "", "hacker_field", "admin",
	}

	for _, field := range blockedFields {
		t.Run("blocked_"+field, func(t *testing.T) {
			err := db.UpdateUsersWithField(field, "value", "uid123")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "not allowed")
		})
	}
}
