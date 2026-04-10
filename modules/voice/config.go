package voice

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
)

const (
	defaultTimeout       = 30
	defaultTotalTimeout  = 45
	defaultMaxDuration   = 60
	defaultMaxFileSize = 3 * 1024 * 1024 // 3MB
)

// Exported constants for voice context limits
const (
	MaxVoiceContextLength = 10000 // max voice correction context characters (rune count)
	MaxContextTextLength  = 10000 // max context_text characters (rune count)
	MaxChatContextLength  = 10000 // max chat_context characters (rune count)
)

var defaultModels = []string{"gemini-3.1-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro"}
var defaultGPTModels = []string{"gpt-4o-mini-transcribe"}

// VoiceConfig holds configuration for voice transcription
type VoiceConfig struct {
	LiteLLMUrl   string
	LiteLLMKey   string
	Timeout      int      // per-model timeout in seconds
	TotalTimeout int      // total timeout across all model fallbacks in seconds
	Models       []string // model fallback chain (Gemini engine)
	MaxDuration  int      // max audio duration in seconds
	MaxFileSize  int64    // max file size in bytes
	Engine       string   // "gemini" or "gpt"
	GPTModels    []string // model fallback chain for GPT engine
	Language     string   // language code for GPT engine, empty = auto-detect
	EditMode     string   // "edit" or "append"
}

// NewVoiceConfigFromEnv reads voice config from environment variables
func NewVoiceConfigFromEnv() *VoiceConfig {
	models := make([]string, len(defaultModels))
	copy(models, defaultModels)
	gptModels := make([]string, len(defaultGPTModels))
	copy(gptModels, defaultGPTModels)

	cfg := &VoiceConfig{
		LiteLLMUrl:   os.Getenv("VOICE_LITELLM_URL"),
		LiteLLMKey:   os.Getenv("VOICE_LITELLM_KEY"),
		Timeout:      defaultTimeout,
		TotalTimeout: defaultTotalTimeout,
		Models:       models,
		MaxDuration:  defaultMaxDuration,
		MaxFileSize:  defaultMaxFileSize,
		Engine:       "gemini",
		GPTModels:    gptModels,
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

	if v := os.Getenv("VOICE_ENGINE"); v == "gpt" || v == "gemini" {
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

	if v := os.Getenv("VOICE_LANGUAGE"); v != "" {
		cfg.Language = v
	}

	// EditMode: explicit setting takes priority, otherwise auto-decide by engine
	if v := os.Getenv("VOICE_EDIT_MODE"); v == "edit" || v == "append" {
		cfg.EditMode = v
	} else {
		if cfg.Engine == "gpt" {
			cfg.EditMode = "append"
		} else {
			cfg.EditMode = "edit"
		}
	}

	// GPT does not support edit mode, force to append
	if cfg.Engine == "gpt" && cfg.EditMode == "edit" {
		lg := log.NewTLog("VoiceConfig")
		lg.Warn("GPT engine does not support edit mode, forcing append")
		cfg.EditMode = "append"
	}

	return cfg
}

// Validate checks that required config fields are set
func (c *VoiceConfig) Validate() error {
	if c.LiteLLMUrl == "" {
		return errors.New("VOICE_LITELLM_URL is required")
	}
	if c.LiteLLMKey == "" {
		return errors.New("VOICE_LITELLM_KEY is required")
	}
	if c.Engine == "gpt" {
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
	"gemini-3.1-pro-preview":  "g31pp",
	"gemini-3-flash-preview":  "g3fp",
	"gemini-2.5-pro":          "g25p",
	"gemini-2.0-flash":        "g20f",
	"gemini-2.0-flash-lite":   "g20fl",
	"gpt-4o-transcribe":       "gpt4ot",
	"gpt-4o-mini-transcribe":  "gpt4omt",
	"whisper-1":               "w1",
	"whisper-large-v3":        "wlv3",
}

// ShortenModelName returns a short identifier for a model name.
// Returns the original name if not in the abbreviation table.
func ShortenModelName(model string) string {
	if short, ok := modelAbbreviations[model]; ok {
		return short
	}
	return model
}
