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
	os.Unsetenv("VOICE_ENGINE")
	os.Unsetenv("VOICE_GPT_MODELS")
	os.Unsetenv("VOICE_LANGUAGE")
	os.Unsetenv("VOICE_EDIT_MODE")
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
	assert.Equal(t, int64(3*1024*1024), cfg.MaxFileSize)
	assert.Equal(t, "gemini", cfg.Engine)
	assert.Equal(t, []string{"gpt-4o-mini-transcribe"}, cfg.GPTModels)
	assert.Equal(t, "", cfg.Language)
	assert.Equal(t, "edit", cfg.EditMode) // gemini defaults to edit
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
	os.Setenv("VOICE_ENGINE", "gpt")
	os.Setenv("VOICE_GPT_MODELS", "gpt-a, gpt-b")
	os.Setenv("VOICE_LANGUAGE", "zh")
	os.Setenv("VOICE_EDIT_MODE", "append")

	cfg := NewVoiceConfigFromEnv()

	assert.Equal(t, "https://litellm.example.com", cfg.LiteLLMUrl)
	assert.Equal(t, "sk-test-key", cfg.LiteLLMKey)
	assert.Equal(t, 20, cfg.Timeout)
	assert.Equal(t, 60, cfg.TotalTimeout)
	assert.Equal(t, []string{"model-a", "model-b", "model-c"}, cfg.Models)
	assert.Equal(t, 120, cfg.MaxDuration)
	assert.Equal(t, int64(10485760), cfg.MaxFileSize)
	assert.Equal(t, "gpt", cfg.Engine)
	assert.Equal(t, []string{"gpt-a", "gpt-b"}, cfg.GPTModels)
	assert.Equal(t, "zh", cfg.Language)
	assert.Equal(t, "append", cfg.EditMode)
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
	assert.Equal(t, int64(3*1024*1024), cfg.MaxFileSize)
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
		Engine:     "gemini",
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}

// --- Engine config tests ---

func TestNewVoiceConfigFromEnv_EngineGPT(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gpt")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "gpt", cfg.Engine)
}

func TestNewVoiceConfigFromEnv_EngineGemini(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gemini")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "gemini", cfg.Engine)
}

func TestNewVoiceConfigFromEnv_EngineInvalid(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "invalid")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "gemini", cfg.Engine)
}

func TestNewVoiceConfigFromEnv_GPTModels(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_GPT_MODELS", "model-x,model-y,model-z")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, []string{"model-x", "model-y", "model-z"}, cfg.GPTModels)
}

func TestNewVoiceConfigFromEnv_GPTModelsWithSpaces(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_GPT_MODELS", " model-x , model-y , model-z ")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, []string{"model-x", "model-y", "model-z"}, cfg.GPTModels)
}

func TestNewVoiceConfigFromEnv_GPTModelsDefault(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, []string{"gpt-4o-mini-transcribe"}, cfg.GPTModels)
}

func TestNewVoiceConfigFromEnv_Language(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_LANGUAGE", "zh")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "zh", cfg.Language)
}

func TestVoiceConfig_Validate_GPTWithModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Engine:     "gpt",
		GPTModels:  []string{"gpt-4o-mini-transcribe"},
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}

func TestVoiceConfig_Validate_GPTWithoutModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Engine:     "gpt",
		GPTModels:  []string{},
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_GPT_MODELS")
}

func TestVoiceConfig_Validate_GeminiWithModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Engine:     "gemini",
		Models:     []string{"gemini-3-flash-preview"},
	}
	err := cfg.Validate()
	assert.NoError(t, err)
}

func TestVoiceConfig_Validate_GeminiWithoutModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Engine:     "gemini",
		Models:     []string{},
	}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_MODELS")
}

// --- EditMode tests ---

func TestEditMode_GeminiDefaultsToEdit(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gemini")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "edit", cfg.EditMode)
}

func TestEditMode_GPTDefaultsToAppend(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gpt")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "append", cfg.EditMode)
}

func TestEditMode_ExplicitAppendOnGemini(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gemini")
	os.Setenv("VOICE_EDIT_MODE", "append")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "append", cfg.EditMode)
}

func TestEditMode_GPTForcedToAppendWhenEditSet(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_ENGINE", "gpt")
	os.Setenv("VOICE_EDIT_MODE", "edit")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "append", cfg.EditMode) // forced to append
}

func TestEditMode_InvalidValueDefaultsByEngine(t *testing.T) {
	clearVoiceEnv()
	defer clearVoiceEnv()

	os.Setenv("VOICE_EDIT_MODE", "invalid")
	cfg := NewVoiceConfigFromEnv()
	assert.Equal(t, "edit", cfg.EditMode) // gemini default

	os.Setenv("VOICE_ENGINE", "gpt")
	cfg = NewVoiceConfigFromEnv()
	assert.Equal(t, "append", cfg.EditMode) // gpt default
}

func TestVoiceConfig_Validate_GPTEngine(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl: "https://example.com",
		LiteLLMKey: "key",
		Engine:     "gpt",
		GPTModels:  []string{"gpt-4o-mini-transcribe"},
	}
	assert.NoError(t, cfg.Validate())

	cfg.GPTModels = []string{}
	err := cfg.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VOICE_GPT_MODELS")
}

// --- TruncateRunes tests ---

func TestTruncateRunes_ShortString(t *testing.T) {
	assert.Equal(t, "hello", TruncateRunes("hello", 10))
}

func TestTruncateRunes_ExactLength(t *testing.T) {
	assert.Equal(t, "hello", TruncateRunes("hello", 5))
}

func TestTruncateRunes_Truncated(t *testing.T) {
	assert.Equal(t, "hel", TruncateRunes("hello", 3))
}

func TestTruncateRunes_CJK(t *testing.T) {
	// Each CJK character is 1 rune
	assert.Equal(t, "你好", TruncateRunes("你好世界", 2))
}

func TestTruncateRunes_Empty(t *testing.T) {
	assert.Equal(t, "", TruncateRunes("", 10))
}

// --- TruncateRunesTail tests ---

func TestTruncateRunesTail_ShortString(t *testing.T) {
	assert.Equal(t, "hello", TruncateRunesTail("hello", 10))
}

func TestTruncateRunesTail_ExactLength(t *testing.T) {
	assert.Equal(t, "hello", TruncateRunesTail("hello", 5))
}

func TestTruncateRunesTail_Truncated(t *testing.T) {
	assert.Equal(t, "llo", TruncateRunesTail("hello", 3))
}

func TestTruncateRunesTail_CJK(t *testing.T) {
	assert.Equal(t, "世界", TruncateRunesTail("你好世界", 2))
}

func TestTruncateRunesTail_Empty(t *testing.T) {
	assert.Equal(t, "", TruncateRunesTail("", 10))
}

func TestTruncateRunesTail_Mixed(t *testing.T) {
	// Mixed ASCII + CJK: each character is 1 rune
	s := "abc你好"
	assert.Equal(t, "好", TruncateRunesTail(s, 1))
	assert.Equal(t, "你好", TruncateRunesTail(s, 2))
	assert.Equal(t, "c你好", TruncateRunesTail(s, 3))
}

// --- ShortenModelName tests ---

func TestShortenModelName_Known(t *testing.T) {
	assert.Equal(t, "g3fp", ShortenModelName("gemini-3-flash-preview"))
	assert.Equal(t, "g31pp", ShortenModelName("gemini-3.1-pro-preview"))
	assert.Equal(t, "g25p", ShortenModelName("gemini-2.5-pro"))
	assert.Equal(t, "gpt4omt", ShortenModelName("gpt-4o-mini-transcribe"))
	assert.Equal(t, "gpt4ot", ShortenModelName("gpt-4o-transcribe"))
	assert.Equal(t, "w1", ShortenModelName("whisper-1"))
	assert.Equal(t, "g20f", ShortenModelName("gemini-2.0-flash"))
}

func TestShortenModelName_Unknown(t *testing.T) {
	assert.Equal(t, "some-new-model", ShortenModelName("some-new-model"))
}

// --- MaxVoiceContextLength constant ---

func TestMaxVoiceContextLength_Constant(t *testing.T) {
	assert.Equal(t, 10000, MaxVoiceContextLength)
}
