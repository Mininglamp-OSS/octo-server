package bot_task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	botTaskEventType    = "bot_task"
	sourcesEnv          = "OCTO_BOT_TASK_SOURCES"
	maxRequestBodyBytes = 256 * 1024
	maxIdentifierBytes  = 256
	maxPromptBytes      = 24 * 1024
	maxContextBytes     = 24 * 1024
	maxMetadataBytes    = 8 * 1024
	maxTaskTypeBytes    = 128
	minSourceTokenBytes = 32
)

type sourceConfig struct {
	Token          string   `json:"token"`
	Enabled        bool     `json:"enabled"`
	AllowedBotUIDs []string `json:"allowed_bot_uids"`
}
type sourceRegistry map[string]sourceConfig

func sourceRegistryFromEnv() (sourceRegistry, error) {
	return parseSourceRegistry(os.Getenv(sourcesEnv))
}

func parseSourceRegistry(raw string) (sourceRegistry, error) {
	if strings.TrimSpace(raw) == "" {
		return sourceRegistry{}, errors.New("OCTO_BOT_TASK_SOURCES not set; bot task ingress disabled")
	}
	var registry sourceRegistry
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registry); err != nil {
		return sourceRegistry{}, fmt.Errorf("decode OCTO_BOT_TASK_SOURCES: %w", err)
	}
	if len(registry) == 0 {
		return sourceRegistry{}, errors.New("OCTO_BOT_TASK_SOURCES is empty; bot task ingress disabled")
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return sourceRegistry{}, errors.New("decode OCTO_BOT_TASK_SOURCES: multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return sourceRegistry{}, fmt.Errorf("decode OCTO_BOT_TASK_SOURCES trailing data: %w", err)
	}
	seenTokens := make(map[string]string, len(registry))
	for source, cfg := range registry {
		trimmedSource := strings.TrimSpace(source)
		cfg.Token = strings.TrimSpace(cfg.Token)
		if trimmedSource == "" || trimmedSource != source || len(source) > maxIdentifierBytes || len(cfg.Token) < minSourceTokenBytes {
			return sourceRegistry{}, fmt.Errorf("invalid bot task source %q", source)
		}
		if other, exists := seenTokens[cfg.Token]; exists {
			return sourceRegistry{}, fmt.Errorf("bot task sources %q and %q share a token", other, source)
		}
		seenTokens[cfg.Token] = source
		registry[source] = cfg
	}
	return registry, nil
}

func (c sourceConfig) allowsBot(uid string) bool {
	for _, allowed := range c.AllowedBotUIDs {
		if allowed = strings.TrimSpace(allowed); allowed == "*" || allowed == uid {
			return true
		}
	}
	return false
}

type taskRequest struct {
	Source         string          `json:"source"`
	TaskType       string          `json:"task_type"`
	IdempotencyKey string          `json:"idempotency_key"`
	BotUID         string          `json:"bot_uid"`
	ActorUID       string          `json:"actor_uid"`
	SessionKey     string          `json:"session_key"`
	Prompt         string          `json:"prompt"`
	Context        json.RawMessage `json:"context"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}
type normalizedTask = taskRequest

type requestValidationError struct{ field string }

func (e *requestValidationError) Error() string {
	return fmt.Sprintf("invalid bot task field %q", e.field)
}

func invalidField(err error) string {
	var validationErr *requestValidationError
	if errors.As(err, &validationErr) {
		return validationErr.field
	}
	return ""
}

func normalizeTaskRequest(req taskRequest) (normalizedTask, error) {
	task := normalizedTask{
		Source: strings.TrimSpace(req.Source), TaskType: strings.TrimSpace(req.TaskType),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), BotUID: strings.TrimSpace(req.BotUID),
		ActorUID: strings.TrimSpace(req.ActorUID), SessionKey: strings.TrimSpace(req.SessionKey), Prompt: req.Prompt,
	}
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"source", task.Source, maxIdentifierBytes}, {"task_type", task.TaskType, maxTaskTypeBytes},
		{"idempotency_key", task.IdempotencyKey, maxIdentifierBytes}, {"bot_uid", task.BotUID, maxIdentifierBytes},
		{"actor_uid", task.ActorUID, maxIdentifierBytes}, {"session_key", task.SessionKey, maxIdentifierBytes},
	} {
		if field.value == "" || len(field.value) > field.max {
			return normalizedTask{}, &requestValidationError{field: field.name}
		}
	}
	if strings.TrimSpace(task.Prompt) == "" || len(task.Prompt) > maxPromptBytes {
		return normalizedTask{}, &requestValidationError{field: "prompt"}
	}
	context, err := normalizeJSONObject(req.Context, true, maxContextBytes)
	if err != nil {
		return normalizedTask{}, &requestValidationError{field: "context"}
	}
	metadata, err := normalizeJSONObject(req.Metadata, false, maxMetadataBytes)
	if err != nil {
		return normalizedTask{}, &requestValidationError{field: "metadata"}
	}
	task.Context, task.Metadata = context, metadata
	return task, nil
}

func normalizeJSONObject(raw json.RawMessage, required bool, max int) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if required {
			return nil, errors.New("required object missing")
		}
		return nil, nil
	}
	if len(raw) > max {
		return nil, errors.New("object too large")
	}
	var value map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("value is not an object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("value contains trailing data")
	}
	return json.Marshal(value)
}

func taskFingerprint(task normalizedTask) string {
	raw, err := json.Marshal(task)
	if err != nil {
		panic(fmt.Sprintf("marshal normalized bot task: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func claimLogHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}
