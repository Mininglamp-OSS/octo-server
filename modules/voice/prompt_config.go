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
	System                   string `yaml:"system"`
	VocabularyReference      string `yaml:"vocabulary_reference"`
	AppendInputBuffer        string `yaml:"append_input_buffer"`
	AppendInputBufferNoVocab string `yaml:"append_input_buffer_no_vocab"`
	EditInputBuffer          string `yaml:"edit_input_buffer"`
	TaskTranscribe           string `yaml:"task_transcribe"`
	TaskTranscribeWithVocab  string `yaml:"task_transcribe_with_vocab"`
	TaskAppend               string `yaml:"task_append"`
	TaskEdit                 string `yaml:"task_edit"`
	TaskEditOnly             string `yaml:"task_edit_only"`

	// Legacy fields: parsed for backward compatibility but ignored with warning.
	Transcribe        string `yaml:"transcribe,omitempty"`
	Modify            string `yaml:"modify,omitempty"`
	AppendContext     string `yaml:"append_context,omitempty"`
	ChatContextSuffix string `yaml:"chat_context_suffix,omitempty"`
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
		System:                   systemPrompt,
		VocabularyReference:      vocabularyReferenceTemplate,
		AppendInputBuffer:        appendInputBufferTemplate,
		AppendInputBufferNoVocab: appendInputBufferNoVocabTemplate,
		EditInputBuffer:          editInputBufferTemplate,
		TaskTranscribe:           taskTranscribe,
		TaskTranscribeWithVocab:  taskTranscribeWithVocab,
		TaskAppend:               taskAppend,
		TaskEdit:                 taskEdit,
		TaskEditOnly:             taskEditOnly,
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

	// Warn about deprecated legacy fields
	legacyFields := []struct {
		name  string
		value string
	}{
		{"transcribe", cfg.Transcribe},
		{"modify", cfg.Modify},
		{"append_context", cfg.AppendContext},
		{"chat_context_suffix", cfg.ChatContextSuffix},
	}
	for _, f := range legacyFields {
		if strings.TrimSpace(f.value) != "" && log != nil {
			log.Warn("deprecated prompt field ignored",
				zap.String("field", f.name),
				zap.String("hint", "use the new v3 fields instead"))
		}
	}

	// System: no placeholder validation
	if strings.TrimSpace(cfg.System) != "" {
		activePrompts.System = strings.TrimRight(cfg.System, "\r\n")
	}

	// Template fields: require exactly 1 %s placeholder
	templateFields := []struct {
		name   string
		value  string
		target *string
	}{
		{"vocabulary_reference", cfg.VocabularyReference, &activePrompts.VocabularyReference},
		{"append_input_buffer", cfg.AppendInputBuffer, &activePrompts.AppendInputBuffer},
		{"append_input_buffer_no_vocab", cfg.AppendInputBufferNoVocab, &activePrompts.AppendInputBufferNoVocab},
		{"edit_input_buffer", cfg.EditInputBuffer, &activePrompts.EditInputBuffer},
	}
	for _, f := range templateFields {
		if strings.TrimSpace(f.value) != "" {
			v := strings.TrimRight(f.value, "\r\n")
			if strings.Count(v, "%s") != 1 {
				if log != nil {
					log.Warn(f.name+" prompt must contain exactly 1 %s placeholder, using default",
						zap.Int("count", strings.Count(v, "%s")))
				}
			} else {
				*f.target = v
			}
		}
	}

	// Task fields: no placeholder validation
	taskFields := []struct {
		name   string
		value  string
		target *string
	}{
		{"task_transcribe", cfg.TaskTranscribe, &activePrompts.TaskTranscribe},
		{"task_transcribe_with_vocab", cfg.TaskTranscribeWithVocab, &activePrompts.TaskTranscribeWithVocab},
		{"task_append", cfg.TaskAppend, &activePrompts.TaskAppend},
		{"task_edit", cfg.TaskEdit, &activePrompts.TaskEdit},
		{"task_edit_only", cfg.TaskEditOnly, &activePrompts.TaskEditOnly},
	}
	for _, f := range taskFields {
		if strings.TrimSpace(f.value) != "" {
			*f.target = strings.TrimRight(f.value, "\r\n")
		}
	}

	if log != nil {
		log.Info("loaded voice prompts from file",
			zap.String("path", filePath),
			zap.String("system", truncatePrompt(activePrompts.System, 80)),
			zap.String("vocabulary_reference", truncatePrompt(activePrompts.VocabularyReference, 80)),
			zap.String("append_input_buffer", truncatePrompt(activePrompts.AppendInputBuffer, 80)),
			zap.String("edit_input_buffer", truncatePrompt(activePrompts.EditInputBuffer, 80)),
			zap.String("task_edit_only", truncatePrompt(activePrompts.TaskEditOnly, 80)),
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
