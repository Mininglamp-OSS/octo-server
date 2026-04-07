package voice

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

const (
	defaultTimeout       = 30
	defaultTotalTimeout  = 45
	defaultMaxDuration   = 60
	defaultMaxFileSize   = 5 * 1024 * 1024 // 5MB
	maxChatContextLength = 10000           // max chat_context characters
)

var defaultModels = []string{"gemini-3.1-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro"}

// VoiceConfig holds configuration for voice transcription
type VoiceConfig struct {
	LiteLLMUrl   string
	LiteLLMKey   string
	Timeout      int      // per-model timeout in seconds
	TotalTimeout int      // total timeout across all model fallbacks in seconds
	Models       []string // model fallback chain
	MaxDuration  int      // max audio duration in seconds
	MaxFileSize  int64    // max file size in bytes
}

// NewVoiceConfigFromEnv reads voice config from environment variables
func NewVoiceConfigFromEnv() *VoiceConfig {
	models := make([]string, len(defaultModels))
	copy(models, defaultModels)

	cfg := &VoiceConfig{
		LiteLLMUrl:   os.Getenv("VOICE_LITELLM_URL"),
		LiteLLMKey:   os.Getenv("VOICE_LITELLM_KEY"),
		Timeout:      defaultTimeout,
		TotalTimeout: defaultTotalTimeout,
		Models:       models,
		MaxDuration:  defaultMaxDuration,
		MaxFileSize:  defaultMaxFileSize,
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
	if len(c.Models) == 0 {
		return errors.New("VOICE_MODELS is required")
	}
	return nil
}
