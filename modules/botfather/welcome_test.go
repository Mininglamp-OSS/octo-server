package botfather

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
)

func TestHandleUserRegisterEvent_SendsWelcomeMessage(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	_, bf := setupTestBotFather(t)

	// Create BotFather user first
	createTestUser(t, bf, BotFatherUID, BotFatherName)

	// Create new user
	newUserUID := "new_user_001"
	createTestUser(t, bf, newUserUID, "New User")

	// Prepare event data
	eventData := []byte(util.ToJson(map[string]interface{}{
		"uid": newUserUID,
	}))

	// Call handler
	var commitErr error
	commitCalled := false
	bf.handleUserRegisterEvent(eventData, func(err error) {
		commitErr = err
		commitCalled = true
	})

	// Verify commit was called without error
	assert.True(t, commitCalled)
	assert.NoError(t, commitErr)

	// Verify welcome sent flag was set in Redis
	sentKey := welcomeSentKeyPrefix + newUserUID
	sentValue, err := bf.ctx.GetRedisConn().GetString(sentKey)
	// Note: err might be "redis: nil" if key doesn't exist, which is OK
	if err == nil || err.Error() != "redis: nil" {
		assert.NoError(t, err)
	}
	assert.NotEmpty(t, sentValue)
}

func TestHandleUserRegisterEvent_Idempotent(t *testing.T) {
	_, bf := setupTestBotFather(t)

	// Create BotFather user
	createTestUser(t, bf, BotFatherUID, BotFatherName)

	// Create new user
	newUserUID := "new_user_002"
	createTestUser(t, bf, newUserUID, "New User 2")

	// Pre-set the welcome sent flag
	sentKey := welcomeSentKeyPrefix + newUserUID
	err := bf.ctx.GetRedisConn().SetAndExpire(sentKey, "1", welcomeSentTTL)
	assert.NoError(t, err)

	// Prepare event data
	eventData := []byte(util.ToJson(map[string]interface{}{
		"uid": newUserUID,
	}))

	// Call handler
	commitCalled := false
	bf.handleUserRegisterEvent(eventData, func(err error) {
		commitCalled = true
	})

	// Should still call commit (idempotent, no error)
	assert.True(t, commitCalled)
}

func TestHandleUserRegisterEvent_SkipsSpecialUsers(t *testing.T) {
	_, bf := setupTestBotFather(t)

	specialUsers := []string{BotFatherUID, "u_10000", "fileHelper"}

	for _, uid := range specialUsers {
		eventData := []byte(util.ToJson(map[string]interface{}{
			"uid": uid,
		}))

		commitCalled := false
		bf.handleUserRegisterEvent(eventData, func(err error) {
			commitCalled = true
		})

		// Should commit without error
		assert.True(t, commitCalled, "commit should be called for %s", uid)

		// No welcome flag should be set
		sentKey := welcomeSentKeyPrefix + uid
		sentValue, _ := bf.ctx.GetRedisConn().GetString(sentKey)
		assert.Empty(t, sentValue, "welcome flag should not be set for %s", uid)
	}
}

func TestHandleUserRegisterEvent_InvalidData(t *testing.T) {
	_, bf := setupTestBotFather(t)

	// Invalid JSON
	eventData := []byte("invalid json")

	commitCalled := false
	var commitErr error
	bf.handleUserRegisterEvent(eventData, func(err error) {
		commitCalled = true
		commitErr = err
	})

	// Should still call commit (don't block on parse error)
	assert.True(t, commitCalled)
	assert.NoError(t, commitErr) // We don't pass error to commit for parse failures
}

func TestHandleUserRegisterEvent_MissingUID(t *testing.T) {
	_, bf := setupTestBotFather(t)

	// Missing uid field
	eventData := []byte(util.ToJson(map[string]interface{}{
		"other_field": "value",
	}))

	commitCalled := false
	bf.handleUserRegisterEvent(eventData, func(err error) {
		commitCalled = true
	})

	// Should call commit without error
	assert.True(t, commitCalled)
}

func TestSendWelcomeMessage(t *testing.T) {
	_, bf := setupTestBotFather(t)

	// Create BotFather user
	createTestUser(t, bf, BotFatherUID, BotFatherName)

	// Create target user
	toUID := "target_user_001"
	createTestUser(t, bf, toUID, "Target User")

	// Note: SendMessageWithResult requires WuKongIM to be running
	// In unit test environment without WuKongIM, this will fail
	// This test mainly verifies the function structure is correct
	err := bf.sendWelcomeMessage(toUID, "")
	// We expect this to fail in test env since WuKongIM is not running
	// Just verify the function doesn't panic
	_ = err
}

func TestWelcomeMessageRenders(t *testing.T) {
	// The welcome message must render in every supported language and keep the
	// stable anchors callers rely on (BotFather identity + the /newbot CTA).
	for _, lang := range octoi18n.SupportedLanguages() {
		got, err := botMessages.Render(MsgWelcome, lang, nil)
		assert.NoError(t, err, "render welcome for %s", lang)
		assert.NotEmpty(t, got, "welcome for %s", lang)
		assert.Contains(t, got, "BotFather", "welcome for %s", lang)
		assert.Contains(t, got, "/newbot", "welcome for %s", lang)
	}
}
