package voice

import (
	"os"
	"strings"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// promptLogger is a minimal logging interface satisfied by both *zap.Logger
// and the project's log.Log / log.TLog types.
type promptLogger interface {
	Info(msg string, fields ...zap.Field)
	Warn(msg string, fields ...zap.Field)
}

// PromptConfig holds configurable prompt templates loaded from file.
// Empty fields fall back to hardcoded defaults in prompt.go.
type PromptConfig struct {
	Transcribe        string `yaml:"transcribe"`
	Modify            string `yaml:"modify"`
	AppendContext     string `yaml:"append_context"`
	ChatContextSuffix string `yaml:"chat_context_suffix"`
}

// activePrompts stores the resolved prompts (file override + defaults).
// It is written once during init() or Route() before any request is served,
// so concurrent reads from HTTP handlers are safe without a mutex.
var activePrompts PromptConfig

func init() {
	resetToDefaults()
}

// resetToDefaults sets activePrompts to the hardcoded constants.
func resetToDefaults() {
	activePrompts = PromptConfig{
		Transcribe:        transcribePrompt,
		Modify:            modifyPromptTemplate,
		AppendContext:     appendContextHint,
		ChatContextSuffix: chatContextSuffix,
	}
}

// LoadPrompts reads prompt templates from a YAML file.
// Missing or empty fields fall back to hardcoded defaults.
// If the file does not exist or fails to parse, all defaults are used.
func LoadPrompts(filePath string, log promptLogger) {
	resetToDefaults()

	if filePath == "" {
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			if log != nil {
				log.Info("voice prompt file not found, using defaults",
					zap.String("path", filePath))
			}
		} else if log != nil {
			log.Warn("failed to read voice prompt file, using defaults",
				zap.String("path", filePath), zap.Error(err))
		}
		return
	}

	var cfg PromptConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		if log != nil {
			log.Warn("failed to parse voice prompt file, using defaults",
				zap.String("path", filePath), zap.Error(err))
		}
		return
	}

	if strings.TrimSpace(cfg.Transcribe) != "" {
		activePrompts.Transcribe = strings.TrimRight(cfg.Transcribe, "\r\n")
	}
	if strings.TrimSpace(cfg.Modify) != "" {
		v := strings.TrimRight(cfg.Modify, "\r\n")
		if strings.Count(v, "%s") != 1 {
			if log != nil {
				log.Warn("modify prompt must contain exactly 1 %s placeholder, using default",
					zap.Int("count", strings.Count(v, "%s")))
			}
		} else {
			activePrompts.Modify = v
		}
	}
	if strings.TrimSpace(cfg.AppendContext) != "" {
		v := strings.TrimRight(cfg.AppendContext, "\r\n")
		if strings.Count(v, "%s") != 1 {
			if log != nil {
				log.Warn("append_context prompt must contain exactly 1 %s placeholder, using default",
					zap.Int("count", strings.Count(v, "%s")))
			}
		} else {
			activePrompts.AppendContext = v
		}
	}
	if strings.TrimSpace(cfg.ChatContextSuffix) != "" {
		v := strings.TrimRight(cfg.ChatContextSuffix, "\r\n")
		if strings.Count(v, "%s") != 1 {
			if log != nil {
				log.Warn("chat_context_suffix prompt must contain exactly 1 %s placeholder, using default",
					zap.Int("count", strings.Count(v, "%s")))
			}
		} else {
			activePrompts.ChatContextSuffix = v
		}
	}

	if log != nil {
		log.Info("loaded voice prompts from file",
			zap.String("path", filePath),
			zap.String("transcribe", truncatePrompt(activePrompts.Transcribe, 80)),
			zap.String("modify", truncatePrompt(activePrompts.Modify, 80)),
			zap.String("append_context", truncatePrompt(activePrompts.AppendContext, 80)),
			zap.String("chat_context_suffix", truncatePrompt(activePrompts.ChatContextSuffix, 80)),
		)
	}
}

// truncatePrompt returns the first n characters of s, appending "..." if truncated.
func truncatePrompt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
