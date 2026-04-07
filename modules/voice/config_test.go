package voice

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func clearVoiceEnv() {
	os.Unsetenv("VOICE_LITELLM_URL")
	os.Unsetenv("VOICE_LITELLM_KEY")
	os.Unsetenv("VOICE_LITELLM_TIMEOUT")
	os.Unsetenv("VOICE_TOTAL_TIMEOUT")
	os.Unsetenv("VOICE_MODELS")
	os.Unsetenv("VOICE_MAX_DURATION")
	os.Unsetenv("VOICE_MAX_FILE_SIZE")
}

func TestNewVoiceConfigFromEnv_Defaults(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	cfg := NewVoiceConfigFromEnv()

	assert.Equal(t, "", cfg.LiteLLMUrl)
	assert.Equal(t, "", cfg.LiteLLMKey)
	assert.Equal(t, 30, cfg.Timeout)
	assert.Equal(t, 45, cfg.TotalTimeout)
	assert.Equal(t, []string{"gemini-3.1-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro"}, cfg.Models)
	assert.Equal(t, 60, cfg.MaxDuration)
	assert.Equal(t, int64(5*1024*1024), cfg.MaxFileSize)
}

func TestNewVoiceConfigFromEnv_AllSet(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_LITELLM_URL", "https://litellm.example.com")
	os.Setenv("VOICE_LITELLM_KEY", "sk-test-key")
	os.Setenv("VOICE_LITELLM_TIMEOUT", "20")
	os.Setenv("VOICE_TOTAL_TIMEOUT", "60")
	os.Setenv("VOICE_MODELS", "model-a, model-b, model-c")
	os.Setenv("VOICE_MAX_DURATION", "120")
	os.Setenv("VOICE_MAX_FILE_SIZE", "10485760")

	cfg := NewVoiceConfigFromEnv()

	assert.Equal(t, "https://litellm.example.com", cfg.LiteLLMUrl)
	assert.Equal(t, "sk-test-key", cfg.LiteLLMKey)
	assert.Equal(t, 20, cfg.Timeout)
	assert.Equal(t, 60, cfg.TotalTimeout)
	assert.Equal(t, []string{"model-a", "model-b", "model-c"}, cfg.Models)
	assert.Equal(t, 120, cfg.MaxDuration)
	assert.Equal(t, int64(10485760), cfg.MaxFileSize)
}

func TestNewVoiceConfigFromEnv_InvalidNumbers(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_LITELLM_TIMEOUT", "invalid")
	os.Setenv("VOICE_TOTAL_TIMEOUT", "-5")
	os.Setenv("VOICE_MAX_DURATION", "abc")
	os.Setenv("VOICE_MAX_FILE_SIZE", "not-a-number")

	cfg := NewVoiceConfigFromEnv()

	// Should keep defaults when values are invalid
	assert.Equal(t, 30, cfg.Timeout)
	assert.Equal(t, 45, cfg.TotalTimeout)
	assert.Equal(t, 60, cfg.MaxDuration)
	assert.Equal(t, int64(5*1024*1024), cfg.MaxFileSize)
}

func TestNewVoiceConfigFromEnv_EmptyModels(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_MODELS", "  ,  ,  ")

	cfg := NewVoiceConfigFromEnv()

	// Should keep default models when all entries are whitespace
	assert.Equal(t, defaultModels, cfg.Models)
}

func TestVoiceConfig_Validate_MissingURL(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMKey: "key",
		Models:     []string{"model"},
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_LITELLM_URL")
}

func TestVoiceConfig_Validate_MissingKey(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		Models:     []string{"model"},
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_LITELLM_KEY")
}

func TestVoiceConfig_Validate_MissingModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Models:     []string{},
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_MODELS")
}

func TestVoiceConfig_Validate_Valid(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Models:     []string{"model-a"},
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}
