package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewASRLogger_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "nested", "path")

	logger := NewASRLogger(subDir, 10)
	require.NotNil(t, logger)
	defer logger.Close()

	_, err := os.Stat(subDir)
	assert.NoError(t, err)
}

func TestNewASRLogger_InvalidDir_ReturnsNil(t *testing.T) {
	// /dev/null is not a directory, so MkdirAll should fail
	logger := NewASRLogger("/dev/null/cannot/create", 10)
	assert.Nil(t, logger)
}

func TestASRLogger_GenerateRequestID(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)
	defer logger.Close()

	id := logger.GenerateRequestID()
	parts := strings.Split(id, "_")
	assert.Len(t, parts, 3)
	// pod_id, unix_ms, random6
	assert.NotEmpty(t, parts[0])
	assert.NotEmpty(t, parts[1])
	assert.Len(t, parts[2], 6) // 3 bytes hex = 6 chars

	// IDs should be unique
	id2 := logger.GenerateRequestID()
	assert.NotEqual(t, id, id2)
}

func TestASRLogger_WriteEntry_JSON_And_Audio(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	ts := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	audioData := []byte("fake-audio-data")
	requestID := "local_1713168000123_a1b2c3"

	entry := ASREntry{
		RequestID: requestID,
		Timestamp: ts.Format(time.RFC3339Nano),
		Source:    "app",
		Engine:    "gemini",
		ModelUsed: "gemini-3.1-pro-preview",
		Input: ASRInput{
			Mode:        "edit",
			MimeType:    "audio/wav",
			AudioSize:   len(audioData),
			ContextText: "existing text",
			ChatContext: "chat history",
		},
		Prompt: &ASRPrompt{
			Type: "chat_completion",
			Text: "prompt text",
			RequestBody: map[string]string{
				"model": "gemini-3.1-pro-preview",
			},
		},
		AudioData:     audioData,
		RawResultText: "raw output",
		ResultText:    "processed output",
		ResultLength:  16,
		IsNoSpeech:    false,
		DurationMs:    1500,
	}

	logger.Enqueue(entry)
	logger.Close()

	// Verify directory structure: {baseDir}/{date}/{engine}/
	engineDir := filepath.Join(dir, "2026-04-15", "gemini")
	_, err := os.Stat(engineDir)
	assert.NoError(t, err)

	// Verify audio file
	audioPath := filepath.Join(engineDir, requestID+".wav")
	audioBytes, err := os.ReadFile(audioPath)
	assert.NoError(t, err)
	assert.Equal(t, audioData, audioBytes)

	// Verify JSON file
	jsonPath := filepath.Join(engineDir, requestID+".json")
	jsonBytes, err := os.ReadFile(jsonPath)
	require.NoError(t, err)

	var stored ASREntry
	err = json.Unmarshal(jsonBytes, &stored)
	require.NoError(t, err)

	assert.Equal(t, requestID, stored.RequestID)
	assert.Equal(t, "app", stored.Source)
	assert.Equal(t, "gemini", stored.Engine)
	assert.Equal(t, "gemini-3.1-pro-preview", stored.ModelUsed)
	assert.Equal(t, "edit", stored.Input.Mode)
	assert.Equal(t, "audio/wav", stored.Input.MimeType)
	assert.Equal(t, len(audioData), stored.Input.AudioSize)
	assert.Equal(t, requestID+".wav", stored.Input.AudioFile)
	assert.Equal(t, "existing text", stored.Input.ContextText)
	assert.Equal(t, "chat history", stored.Input.ChatContext)
	assert.Equal(t, "chat_completion", stored.Prompt.Type)
	assert.Equal(t, "prompt text", stored.Prompt.Text)
	assert.Equal(t, "raw output", stored.RawResultText)
	assert.Equal(t, "processed output", stored.ResultText)
	assert.Equal(t, 16, stored.ResultLength)
	assert.False(t, stored.IsNoSpeech)
	assert.Equal(t, int64(1500), stored.DurationMs)
	assert.Empty(t, stored.Error)
	// AudioData should not be serialized to JSON
	assert.Nil(t, stored.AudioData)
}

func TestASRLogger_WriteEntry_NoAudio(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	entry := ASREntry{
		RequestID: "local_123_abc",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Source:    "bot",
		Engine:    "gpt",
		Error:     "some error",
	}

	logger.Enqueue(entry)
	logger.Close()

	// Only JSON should exist, no audio file
	dateDir := time.Now().UTC().Format("2006-01-02")
	engineDir := filepath.Join(dir, dateDir, "gpt")

	jsonPath := filepath.Join(engineDir, "local_123_abc.json")
	_, err := os.Stat(jsonPath)
	assert.NoError(t, err)

	// No audio file should exist
	entries, _ := os.ReadDir(engineDir)
	jsonCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonCount++
		}
	}
	assert.Equal(t, 1, jsonCount)
	assert.Equal(t, 1, len(entries)) // only json, no audio
}

func TestASRLogger_MultipleEngines(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	ts := time.Now().UTC().Format(time.RFC3339Nano)

	logger.Enqueue(ASREntry{
		RequestID: "id_1", Timestamp: ts, Engine: "gemini",
		AudioData: []byte("a"), Input: ASRInput{MimeType: "audio/wav"},
	})
	logger.Enqueue(ASREntry{
		RequestID: "id_2", Timestamp: ts, Engine: "gpt",
		AudioData: []byte("b"), Input: ASRInput{MimeType: "audio/mp3"},
	})
	logger.Enqueue(ASREntry{
		RequestID: "id_3", Timestamp: ts, Engine: "qwen",
		AudioData: []byte("c"), Input: ASRInput{MimeType: "audio/wav"},
	})

	logger.Close()

	dateDir := time.Now().UTC().Format("2006-01-02")
	for _, engine := range []string{"gemini", "gpt", "qwen"} {
		engineDir := filepath.Join(dir, dateDir, engine)
		_, err := os.Stat(engineDir)
		assert.NoError(t, err, "engine dir should exist: %s", engine)
	}
}

func TestASRLogger_GracefulClose_DrainEntries(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 100)
	require.NotNil(t, logger)

	// Enqueue several entries
	for i := 0; i < 5; i++ {
		logger.Enqueue(ASREntry{
			RequestID: logger.GenerateRequestID(),
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Engine:    "gemini",
			AudioData: []byte("audio"),
			Input:     ASRInput{MimeType: "audio/wav"},
		})
	}

	// Close should drain all entries
	logger.Close()

	dateDir := time.Now().UTC().Format("2006-01-02")
	engineDir := filepath.Join(dir, dateDir, "gemini")
	entries, err := os.ReadDir(engineDir)
	require.NoError(t, err)

	// Each entry produces 2 files (audio + json)
	assert.Equal(t, 10, len(entries))
}

func TestASRLogger_Enqueue_AfterClose_Drops(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	logger.Close()

	// Enqueue after close should not panic and should be silently dropped
	logger.Enqueue(ASREntry{
		RequestID: "should_be_dropped",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Engine:    "gemini",
		AudioData: []byte("audio"),
		Input:     ASRInput{MimeType: "audio/wav"},
	})

	// Verify no files were written
	dateDir := time.Now().UTC().Format("2006-01-02")
	engineDir := filepath.Join(dir, dateDir, "gemini")
	_, err := os.Stat(engineDir)
	assert.True(t, os.IsNotExist(err))
}

func TestASRLogger_BufferFull_Drops(t *testing.T) {
	dir := t.TempDir()
	// Create logger with very small buffer
	logger := &ASRLogger{
		baseDir: dir,
		podID:   "test",
		ch:      make(chan ASREntry, 1),
		Log:     log.NewTLog("ASRLog"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger.ctx = ctx
	logger.cancel = cancel

	// Fill the buffer without starting worker
	logger.ch <- ASREntry{RequestID: "first"}

	// Next enqueue should be dropped (buffer full)
	logger.Enqueue(ASREntry{RequestID: "should_drop"})

	// Channel should still have just the first entry
	assert.Len(t, logger.ch, 1)
	cancel()
}

func TestASRLogger_MimeTypeFormats(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	tests := []struct {
		mime     string
		expected string
	}{
		{"audio/wav", "wav"},
		{"audio/mp3", "mp3"},
		{"audio/ogg", "ogg"},
		{"audio/webm", "webm"},
		{"audio/mp4", "m4a"},
		{"audio/flac", "flac"},
	}

	for _, tt := range tests {
		id := logger.GenerateRequestID()
		logger.Enqueue(ASREntry{
			RequestID: id,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Engine:    "gemini",
			AudioData: []byte("audio"),
			Input:     ASRInput{MimeType: tt.mime},
		})
	}

	logger.Close()

	dateDir := time.Now().UTC().Format("2006-01-02")
	engineDir := filepath.Join(dir, dateDir, "gemini")
	entries, err := os.ReadDir(engineDir)
	require.NoError(t, err)

	// Each produces 2 files
	assert.Equal(t, len(tests)*2, len(entries))

	// Check that audio files have correct extensions
	for _, tt := range tests {
		found := false
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), "."+tt.expected) {
				found = true
				break
			}
		}
		assert.True(t, found, "should have file with extension .%s for mime %s", tt.expected, tt.mime)
	}
}

func TestASRLogger_PodIDFromHostname(t *testing.T) {
	dir := t.TempDir()
	// Save and restore HOSTNAME
	oldHostname := os.Getenv("HOSTNAME")
	defer os.Setenv("HOSTNAME", oldHostname)

	os.Setenv("HOSTNAME", "pod-abc")
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)
	defer logger.Close()

	id := logger.GenerateRequestID()
	assert.True(t, strings.HasPrefix(id, "pod-abc_"))
}

func TestASRLogger_PodIDDefaultsToLocal(t *testing.T) {
	dir := t.TempDir()
	oldHostname := os.Getenv("HOSTNAME")
	defer os.Setenv("HOSTNAME", oldHostname)

	os.Setenv("HOSTNAME", "")
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)
	defer logger.Close()

	id := logger.GenerateRequestID()
	assert.True(t, strings.HasPrefix(id, "local_"))
}

func TestASRLogger_GPTRequestBody(t *testing.T) {
	dir := t.TempDir()
	logger := NewASRLogger(dir, 10)
	require.NotNil(t, logger)

	entry := ASREntry{
		RequestID: "gpt_test_1",
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Source:    "bot",
		Engine:    "gpt",
		ModelUsed: "gpt-4o-mini-transcribe",
		Input: ASRInput{
			Mode:     "append",
			MimeType: "audio/wav",
		},
		Prompt: &ASRPrompt{
			Type: "audio_transcription",
			Text: "prompt text",
			RequestBody: map[string]interface{}{
				"model":        "gpt-4o-mini-transcribe",
				"language":     "zh",
				"prompt":       "prompt text",
				"file":         "(multipart binary, see audio_file in input)",
				"audio_base64": "dGVzdA==",
			},
		},
		AudioData:     []byte("test"),
		RawResultText: "result",
		ResultText:    "result",
		ResultLength:  6,
	}

	logger.Enqueue(entry)
	logger.Close()

	dateDir := time.Now().UTC().Format("2006-01-02")
	jsonPath := filepath.Join(dir, dateDir, "gpt", "gpt_test_1.json")
	jsonBytes, err := os.ReadFile(jsonPath)
	require.NoError(t, err)

	var stored map[string]interface{}
	err = json.Unmarshal(jsonBytes, &stored)
	require.NoError(t, err)

	prompt := stored["prompt"].(map[string]interface{})
	assert.Equal(t, "audio_transcription", prompt["type"])
	reqBody := prompt["request_body"].(map[string]interface{})
	assert.Equal(t, "dGVzdA==", reqBody["audio_base64"])
}
