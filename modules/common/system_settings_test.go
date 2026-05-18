package common

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to construct a SystemSettings backed by the test DB plus the given
// yaml-side defaults applied to the context's config.
func newTestSystemSettings(t *testing.T, apply func(s *SystemSettings)) *SystemSettings {
	t.Helper()
	// Defensive reset: key_encryption_test.go intentionally Unsetenvs the
	// master key without restoring it, so any test running after it would
	// panic when NewTestServer triggers RSA private-key encryption. Reset
	// here so test order is irrelevant.
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	db := newSystemSettingDB(ctx)
	s := NewSystemSettings(ctx, db)
	require.NoError(t, s.Load())
	if apply != nil {
		apply(s)
	}
	return s
}

func TestSystemSettings_BoolFallsBackToYamlWhenUnset(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = true
	s.ctx.GetConfig().Register.Off = false
	require.NoError(t, s.Reload())

	assert.True(t, s.RegisterEmailOn(), "DB empty -> fall back to yaml true")
	assert.False(t, s.RegisterOff(), "DB empty -> fall back to yaml false")
}

func TestSystemSettings_BoolOverridesYamlWhenSet(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = true // yaml says on
	s.ctx.GetConfig().Register.Off = false    // yaml says open

	// Admin disables both via DB.
	require.NoError(t, s.db.upsert("register", "email_on", "0", settingTypeBool, ""))
	require.NoError(t, s.db.upsert("register", "off", "1", settingTypeBool, ""))
	require.NoError(t, s.Reload())

	assert.False(t, s.RegisterEmailOn(), "DB 0 must override yaml true")
	assert.True(t, s.RegisterOff(), "DB 1 must override yaml false")
}

func TestSystemSettings_StringFallsBackOnEmpty(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailSmtp = "smtp.yaml.example:465"

	// No DB row -> yaml fallback.
	assert.Equal(t, "smtp.yaml.example:465", s.SupportEmailSmtp())

	// Empty DB value still triggers fallback (treated as "not configured").
	require.NoError(t, s.db.upsert("support", "email_smtp", "", settingTypeString, ""))
	require.NoError(t, s.Reload())
	assert.Equal(t, "smtp.yaml.example:465", s.SupportEmailSmtp())

	// Non-empty DB value wins.
	require.NoError(t, s.db.upsert("support", "email_smtp", "smtp.db.example:587", settingTypeString, ""))
	require.NoError(t, s.Reload())
	assert.Equal(t, "smtp.db.example:587", s.SupportEmailSmtp())
}

func TestSystemSettings_EncryptedRoundTrip(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailPwd = "yaml-fallback"

	// Store encrypted; helper must decrypt on read.
	enc, err := encryptKey("real-smtp-password")
	require.NoError(t, err)
	require.NoError(t, s.db.upsert("support", "email_pwd", enc, settingTypeEncrypted, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, "real-smtp-password", s.SupportEmailPwd())
}

func TestSystemSettings_EncryptedDecryptFailureFallsBackToYaml(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Support.EmailPwd = "yaml-pwd"

	// Corrupted ciphertext (enc: prefix but invalid body).
	require.NoError(t, s.db.upsert("support", "email_pwd", "enc:not-real-base64", settingTypeEncrypted, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, "yaml-pwd", s.SupportEmailPwd(), "decryption failure must fall back to yaml, not panic")
}

func TestSystemSettings_ReloadRefreshesSnapshot(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = false

	require.NoError(t, s.db.upsert("register", "email_on", "1", settingTypeBool, ""))
	// Before reload, snapshot still empty -> yaml.
	assert.False(t, s.RegisterEmailOn())

	require.NoError(t, s.Reload())
	assert.True(t, s.RegisterEmailOn())
}

func TestSystemSettings_ConcurrentReadsAndReloads(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	s.ctx.GetConfig().Register.EmailOn = false
	require.NoError(t, s.db.upsert("register", "email_on", "1", settingTypeBool, ""))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = s.RegisterEmailOn()
				_ = s.SupportEmailSmtp()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Reload()
			}
		}()
	}
	wg.Wait()
}
