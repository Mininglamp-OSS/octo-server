package voice

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newTestConfig(serverURL string) *VoiceConfig {
	return &VoiceConfig{
		LiteLLMUrl:   serverURL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"model-a", "model-b", "model-c"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
	}
}

func TestTranscribe_Success_FirstModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, "model-a", req.Model)

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Hello world"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "Hello world", text)
	assert.Equal(t, "model-a", model)
}

func TestTranscribe_FallbackToSecondModel(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		if count == 1 {
			// First model fails with 500
			assert.Equal(t, "model-a", req.Model)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "internal error"}`))
			return
		}

		// Second model succeeds
		assert.Equal(t, "model-b", req.Model)
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Fallback result"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "Fallback result", text)
	assert.Equal(t, "model-b", model)
}

func TestTranscribe_NoRetryOn4xx(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.Error(t, err)
	// Should not retry on 400 - only 1 call
	assert.Equal(t, int32(1), atomic.LoadInt32(&callCount))
}

func TestTranscribe_RetryOn429(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		if count <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limited"}`))
			return
		}

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Finally!"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "Finally!", text)
	assert.Equal(t, "model-c", model)
	assert.Equal(t, int32(3), atomic.LoadInt32(&callCount))
}

func TestTranscribe_AllModelsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "server error"}`))
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "all models failed")
}

func TestTranscribe_TotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "too late"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Timeout = 2
	cfg.TotalTimeout = 3
	svc := NewVoiceService(cfg)

	start := time.Now()
	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	elapsed := time.Since(start)

	assert.Error(t, err)
	// Should complete within total timeout + some margin
	assert.True(t, elapsed < 5*time.Second, "should respect total timeout")
}

func TestTranscribe_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{Choices: []choice{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty response")
}

func TestTranscribe_WithContextText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		// Verify the prompt contains context text
		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "已有文本")
		assert.Contains(t, prompt, "existing text here")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "modified text"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "existing text here", "")
	assert.NoError(t, err)
	assert.Equal(t, "modified text", text)
}

func TestTranscribe_WithChatContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "以下是当前聊天的最近对话记录")
		assert.Contains(t, prompt, "Alice: 周五开会")
		assert.Contains(t, prompt, "准确转写为文字")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "transcribed with context"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "Alice: 周五开会")
	assert.NoError(t, err)
	assert.Equal(t, "transcribed with context", text)
}

func TestTranscribe_WithChatContextAndContextText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "以下是当前聊天的最近对话记录")
		assert.Contains(t, prompt, "chat history here")
		assert.Contains(t, prompt, "已有文本")
		assert.Contains(t, prompt, "existing draft")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "modified with context"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "existing draft", "chat history here")
	assert.NoError(t, err)
	assert.Equal(t, "modified with context", text)
}

func TestMimeTypeToFormat(t *testing.T) {
	tests := []struct {
		mimeType string
		expected string
	}{
		{"audio/wav", "wav"},
		{"audio/x-wav", "wav"},
		{"audio/mp3", "mp3"},
		{"audio/mpeg", "mp3"},
		{"audio/ogg", "ogg"},
		{"audio/webm", "webm"},
		{"audio/mp4", "m4a"},
		{"audio/x-m4a", "m4a"},
		{"audio/flac", "flac"},
		{"application/octet-stream", "wav"},
		{"unknown/type", "wav"},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			assert.Equal(t, tt.expected, mimeTypeToFormat(tt.mimeType))
		})
	}
}

func TestIsNonRetryableError(t *testing.T) {
	assert.True(t, isNonRetryableError(&apiError{StatusCode: 400}))
	assert.True(t, isNonRetryableError(&apiError{StatusCode: 401}))
	assert.True(t, isNonRetryableError(&apiError{StatusCode: 403}))
	assert.False(t, isNonRetryableError(&apiError{StatusCode: 429}))
	assert.False(t, isNonRetryableError(&apiError{StatusCode: 500}))
	assert.False(t, isNonRetryableError(&apiError{StatusCode: 502}))
	assert.False(t, isNonRetryableError(assert.AnError))
}

func TestApiError_Error(t *testing.T) {
	err := &apiError{StatusCode: 500, Body: "internal error"}
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "internal error")
}
