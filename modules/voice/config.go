package voice

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

// Engine name constants
const (
	EngineGemini = "gemini"
	EngineGPT    = "gpt"
	EngineQwen   = "qwen"
)

const (
	defaultTimeout      = 30
	defaultTotalTimeout = 45
	defaultMaxDuration  = 60
	defaultMaxFileSize  = 3 * 1024 * 1024 // 3MB
)

// Package-private default values for voice context limits
const (
	defaultMaxVoiceContextLength  = 10000
	defaultMaxContextTextLength   = 5000
	defaultMaxChatContextLength   = 20000
	defaultMaxMemberContextLength = 5000
)

var defaultModels = []string{"gemini-3.1-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro"}
var defaultGPTModels = []string{"gpt-4o-mini-transcribe"}
var defaultQwenModels = []string{"qwen3.5-omni-plus"}

// VoiceConfig holds configuration for voice transcription
type VoiceConfig struct {
	LiteLLMUrl   string
	LiteLLMKey   string
	Timeout      int      // per-model timeout in seconds
	TotalTimeout int      // total timeout across all model fallbacks in seconds
	Models       []string // model fallback chain (Gemini engine)
	MaxDuration  int      // max audio duration in seconds
	MaxFileSize  int64    // max file size in bytes
	Engine       string   // "gemini", "gpt", or "qwen"
	GPTModels    []string // model fallback chain for GPT engine
	Language     string   // language code for GPT engine, empty = auto-detect
	EditMode     string   // "edit" or "append"

	// Qwen engine configuration
	QwenModels  []string // Qwen engine model fallback chain
	QwenUrl     string   // Qwen-specific API URL (optional, falls back to LiteLLMUrl)
	QwenKey     string   // Qwen-specific API key (optional, falls back to LiteLLMKey)
	QwenTimeout int      // Qwen per-model timeout (optional, falls back to Timeout)

	PromptFile string // Path to voice_prompts.yaml (env: VOICE_PROMPT_FILE)

	EmotionEmoji bool // Enable emotion emoji annotation in output (env: VOICE_EMOTION_EMOJI, default: true)

	LocalEnabled       bool   // 是否允许前端使用本地模型（env: VOICE_LOCAL_ENABLED, default: true）
	LocalTimeoutMs     int    // 本地模型请求超时毫秒（env: VOICE_LOCAL_TIMEOUT_MS, default: 10000）
	LocalProbeURL      string // Full URL for local model health probe (env: VOICE_LOCAL_PROBE_URL, default: "http://localhost:8787/")
	LocalTranscribeURL string // Full URL for local model transcription (env: VOICE_LOCAL_TRANSCRIBE_URL, default: "http://localhost:8787/v1/voice/transcribe")

	FeedbackURL string // Voice quality feedback service URL (env: VOICE_FEEDBACK_URL)

	MaxVoiceContextLength  int // max personal voice context runes (env: VOICE_MAX_VOICE_CONTEXT_LENGTH)
	MaxContextTextLength   int // max context_text runes (env: VOICE_MAX_CONTEXT_TEXT_LENGTH)
	MaxChatContextLength   int // max chat_context runes (env: VOICE_MAX_CHAT_CONTEXT_LENGTH)
	MaxMemberContextLength int // max member_context runes (env: VOICE_MAX_MEMBER_CONTEXT_LENGTH)
}

// NewVoiceConfigFromEnv reads voice config from environment variables
func NewVoiceConfigFromEnv() *VoiceConfig {
	models := make([]string, len(defaultModels))
	copy(models, defaultModels)
	gptModels := make([]string, len(defaultGPTModels))
	copy(gptModels, defaultGPTModels)
	qwenModels := make([]string, len(defaultQwenModels))
	copy(qwenModels, defaultQwenModels)

	cfg := &VoiceConfig{
		LiteLLMUrl:   os.Getenv("VOICE_LITELLM_URL"),
		LiteLLMKey:   os.Getenv("VOICE_LITELLM_KEY"),
		Timeout:      defaultTimeout,
		TotalTimeout: defaultTotalTimeout,
		Models:       models,
		MaxDuration:  defaultMaxDuration,
		MaxFileSize:  defaultMaxFileSize,
		Engine:       EngineGemini,
		GPTModels:    gptModels,
		QwenModels:   qwenModels,
	}

	if v := os.Getenv("VOICE_LITELLM_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Timeout = n
		}
	}

	if v := os.Getenv("VOICE_TOTAL_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TotalTimeout = n
		}
	}

	if v := os.Getenv("VOICE_MODELS"); v != "" {
		models := strings.Split(v, ",")
		trimmed := make([]string, 0, len(models))
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m != "" {
				trimmed = append(trimmed, m)
			}
		}
		if len(trimmed) > 0 {
			cfg.Models = trimmed
		}
	}

	if v := os.Getenv("VOICE_MAX_DURATION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxDuration = n
		}
	}

	if v := os.Getenv("VOICE_MAX_FILE_SIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxFileSize = n
		}
	}

	if v := os.Getenv("VOICE_ENGINE"); v == EngineGPT || v == EngineGemini || v == EngineQwen {
		cfg.Engine = v
	}

	if v := os.Getenv("VOICE_GPT_MODELS"); v != "" {
		models := strings.Split(v, ",")
		trimmed := make([]string, 0, len(models))
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m != "" {
				trimmed = append(trimmed, m)
			}
		}
		if len(trimmed) > 0 {
			cfg.GPTModels = trimmed
		}
	}

	// Qwen engine configuration
	if v := os.Getenv("VOICE_QWEN_MODELS"); v != "" {
		models := strings.Split(v, ",")
		trimmed := make([]string, 0, len(models))
		for _, m := range models {
			m = strings.TrimSpace(m)
			if m != "" {
				trimmed = append(trimmed, m)
			}
		}
		if len(trimmed) > 0 {
			cfg.QwenModels = trimmed
		}
	}

	if v := os.Getenv("VOICE_QWEN_URL"); v != "" {
		cfg.QwenUrl = v
	}

	if v := os.Getenv("VOICE_QWEN_KEY"); v != "" {
		cfg.QwenKey = v
	}

	if v := os.Getenv("VOICE_QWEN_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.QwenTimeout = n
		}
	}

	if v := os.Getenv("VOICE_LANGUAGE"); v != "" {
		cfg.Language = v
	}

	if v := os.Getenv("VOICE_PROMPT_FILE"); v != "" {
		cfg.PromptFile = v
	}

	cfg.EmotionEmoji = true
	if v := os.Getenv("VOICE_EMOTION_EMOJI"); v == "false" || v == "0" {
		cfg.EmotionEmoji = false
	}

	cfg.MaxVoiceContextLength = getEnvInt("VOICE_MAX_VOICE_CONTEXT_LENGTH", defaultMaxVoiceContextLength)
	cfg.MaxContextTextLength = getEnvInt("VOICE_MAX_CONTEXT_TEXT_LENGTH", defaultMaxContextTextLength)
	cfg.MaxChatContextLength = getEnvInt("VOICE_MAX_CHAT_CONTEXT_LENGTH", defaultMaxChatContextLength)
	cfg.MaxMemberContextLength = getEnvInt("VOICE_MAX_MEMBER_CONTEXT_LENGTH", defaultMaxMemberContextLength)

	// EditMode: explicit setting takes priority, otherwise auto-decide by engine
	if v := os.Getenv("VOICE_EDIT_MODE"); v == "edit" || v == "append" {
		cfg.EditMode = v
	} else {
		if cfg.Engine == EngineGPT {
			cfg.EditMode = "append"
		} else {
			cfg.EditMode = "edit"
		}
	}

	// GPT does not support edit mode, force to append
	if cfg.Engine == EngineGPT && cfg.EditMode == "edit" {
		lg := log.NewTLog("VoiceConfig")
		lg.Warn("GPT engine does not support edit mode, forcing append")
		cfg.EditMode = "append"
	}

	cfg.LocalEnabled = true
	if v := os.Getenv("VOICE_LOCAL_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.LocalEnabled = b
		}
	}

	cfg.LocalTimeoutMs = 10000
	if v := os.Getenv("VOICE_LOCAL_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 60000 {
				n = 60000
			}
			cfg.LocalTimeoutMs = n
		}
	}

	cfg.LocalProbeURL = "http://localhost:8787/"
	if v := os.Getenv("VOICE_LOCAL_PROBE_URL"); v != "" {
		cfg.LocalProbeURL = v
	}

	cfg.LocalTranscribeURL = "http://localhost:8787/v1/voice/transcribe"
	if v := os.Getenv("VOICE_LOCAL_TRANSCRIBE_URL"); v != "" {
		cfg.LocalTranscribeURL = v
	}

	if v := os.Getenv("VOICE_FEEDBACK_URL"); v != "" {
		cfg.FeedbackURL = v
	}

	return cfg
}

// Validate checks that required config fields are set
func (c *VoiceConfig) Validate() error {
	if c.Engine == EngineQwen {
		// Qwen can use its own URL/Key or fall back to global
		url := c.QwenUrl
		if url == "" {
			url = c.LiteLLMUrl
		}
		if url == "" {
			return errors.New("VOICE_QWEN_URL or VOICE_LITELLM_URL is required for qwen engine")
		}
		key := c.QwenKey
		if key == "" {
			key = c.LiteLLMKey
		}
		if key == "" {
			return errors.New("VOICE_QWEN_KEY or VOICE_LITELLM_KEY is required for qwen engine")
		}
		if len(c.QwenModels) == 0 {
			return errors.New("VOICE_QWEN_MODELS is required for qwen engine")
		}
		return nil
	}

	// Original validation for gemini/gpt
	if c.LiteLLMUrl == "" {
		return errors.New("VOICE_LITELLM_URL is required")
	}
	if c.LiteLLMKey == "" {
		return errors.New("VOICE_LITELLM_KEY is required")
	}
	if c.Engine == EngineGPT {
		if len(c.GPTModels) == 0 {
			return errors.New("VOICE_GPT_MODELS is required for GPT engine")
		}
	} else {
		if len(c.Models) == 0 {
			return errors.New("VOICE_MODELS is required")
		}
	}
	return nil
}

func getEnvInt(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}

// TruncateRunes truncates a string to at most max Unicode characters (rune-safe).
func TruncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// TruncateRunesTail keeps the last max Unicode characters of a string (rune-safe).
// Used for context_text / chat_context truncation to preserve recent content.
func TruncateRunesTail(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[len(runes)-max:])
}

// modelAbbreviations maps full model names to short identifiers
var modelAbbreviations = map[string]string{
	"gemini-3.1-pro-preview": "g31pp",
	"gemini-3-flash-preview": "g3fp",
	"gemini-2.5-pro":         "g25p",
	"gemini-2.0-flash":       "g20f",
	"gemini-2.0-flash-lite":  "g20fl",
	"gpt-4o-transcribe":      "gpt4ot",
	"gpt-4o-mini-transcribe": "gpt4omt",
	"whisper-1":              "w1",
	"whisper-large-v3":       "wlv3",
	"qwen3.5-omni-plus":      "q35op",
}

// ShortenModelName returns a short identifier for a model name.
// Returns the original name if not in the abbreviation table.
func ShortenModelName(model string) string {
	if short, ok := modelAbbreviations[model]; ok {
		return short
	}
	return model
}
