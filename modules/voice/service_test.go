package voice

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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
		GPTModels:    []string{"gpt-a", "gpt-b"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		Engine:       "gemini",
		EditMode:     "edit",
	}
}

func newGPTTestConfig(serverURL string) *VoiceConfig {
	return &VoiceConfig{
		LiteLLMUrl:   serverURL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		GPTModels:    []string{"gpt-model-a", "gpt-model-b"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		Engine:       "gpt",
		EditMode:     "append",
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

		// In edit mode, prompt uses modifyPromptTemplate
		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "已有以下文本")
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
		assert.Contains(t, prompt, "辅助识别专有名词拼写")
		assert.Contains(t, prompt, "Alice: 周五开会")
		assert.Contains(t, prompt, "准确还原说话内容")

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
		assert.Contains(t, prompt, "辅助识别专有名词拼写")
		assert.Contains(t, prompt, "chat history here")
		assert.Contains(t, prompt, "已有以下文本")
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

// --- GPT engine tests ---

func parseMultipartForm(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	contentType := r.Header.Get("Content-Type")
	_, params, err := mime.ParseMediaType(contentType)
	assert.NoError(t, err)
	mr := multipart.NewReader(r.Body, params["boundary"])
	fields := make(map[string]string)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		assert.NoError(t, err)
		name := part.FormName()
		if name == "file" {
			data, _ := io.ReadAll(part)
			fields["file"] = string(data)
			fields["file_name"] = part.FileName()
		} else {
			data, _ := io.ReadAll(part)
			fields[name] = string(data)
		}
	}
	return fields
}

func TestGPT_Transcribe_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/audio/transcriptions", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		fields := parseMultipartForm(t, r)
		assert.Equal(t, "gpt-model-a", fields["model"])
		assert.Equal(t, "fake-audio", fields["file"])

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "GPT transcribed"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "GPT transcribed", text)
	assert.Equal(t, "gpt-model-a", model)
}

func TestGPT_Transcribe_ModelFallback(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		fields := parseMultipartForm(t, r)

		if count == 1 {
			assert.Equal(t, "gpt-model-a", fields["model"])
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "internal error"}`))
			return
		}

		assert.Equal(t, "gpt-model-b", fields["model"])
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "Fallback GPT result"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "Fallback GPT result", text)
	assert.Equal(t, "gpt-model-b", model)
}

func TestGPT_Transcribe_NoRetryOn4xx(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&callCount))
}

func TestGPT_Transcribe_RetryOn429(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		if count == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limited"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "After rate limit"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "After rate limit", text)
	assert.Equal(t, "gpt-model-b", model)
	assert.Equal(t, int32(2), atomic.LoadInt32(&callCount))
}

func TestGPT_Transcribe_AllModelsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "server error"}`))
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "all GPT models failed")
}

func TestGPT_Transcribe_NoSpeech_EmptyText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": ""})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "", text)
	assert.Equal(t, "gpt-model-a", model)
}

func TestGPT_Transcribe_NoSpeech_Sentinel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "[NO_SPEECH]"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "", text)
	assert.Equal(t, "gpt-model-a", model)
}

func TestGPT_Transcribe_WithContextText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := parseMultipartForm(t, r)
		// In append mode, prompt uses appendContextPromptTemplate
		assert.Contains(t, fields["prompt"], "辅助理解语境和专有名词纠错")
		assert.Contains(t, fields["prompt"], "existing text here")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "new words"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "existing text here", "")
	assert.NoError(t, err)
	assert.Equal(t, "existing text here new words", text) // append join with space
}

func TestGPT_Transcribe_WithChatContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := parseMultipartForm(t, r)
		assert.Contains(t, fields["prompt"], "Alice: 周五开会")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "transcribed with context"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "Alice: 周五开会")
	assert.NoError(t, err)
	assert.Equal(t, "transcribed with context", text)
}

func TestGPT_Transcribe_TotalTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "too late"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	cfg.Timeout = 2
	cfg.TotalTimeout = 3
	svc := NewVoiceService(cfg)

	start := time.Now()
	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.True(t, elapsed < 5*time.Second, "should respect total timeout")
}

func TestGPT_Transcribe_MultipartFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := parseMultipartForm(t, r)
		assert.Equal(t, "gpt-model-a", fields["model"])
		assert.Equal(t, "zh", fields["language"])
		assert.NotEmpty(t, fields["prompt"])
		assert.Equal(t, "fake-audio", fields["file"])
		assert.True(t, strings.HasSuffix(fields["file_name"], ".wav"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "OK"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-model-a"}
	cfg.Language = "zh"
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "OK", text)
}

func TestGPT_Transcribe_NoLanguageWhenEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields := parseMultipartForm(t, r)
		_, hasLanguage := fields["language"]
		assert.False(t, hasLanguage, "language field should not be present when empty")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "OK"})
	}))
	defer server.Close()

	cfg := newGPTTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-model-a"}
	cfg.Language = ""
	svc := NewVoiceService(cfg)

	_, _, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
}

func TestGemini_Transcribe_ExplicitEngine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Gemini result"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Engine = "gemini"
	cfg.Models = []string{"gemini-model"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("fake-audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "Gemini result", text)
	assert.Equal(t, "gemini-model", model)
}

// --- joinContextAndText tests ---

func TestJoinContextAndText_BothEmpty(t *testing.T) {
	assert.Equal(t, "", joinContextAndText("", ""))
}

func TestJoinContextAndText_EmptyContext(t *testing.T) {
	assert.Equal(t, "hello", joinContextAndText("", "hello"))
}

func TestJoinContextAndText_EmptyNewText(t *testing.T) {
	assert.Equal(t, "hello", joinContextAndText("hello", ""))
}

func TestJoinContextAndText_CJK(t *testing.T) {
	assert.Equal(t, "你好世界", joinContextAndText("你好", "世界"))
}

func TestJoinContextAndText_English(t *testing.T) {
	assert.Equal(t, "Hello world", joinContextAndText("Hello", "world"))
}

func TestJoinContextAndText_CJKToEnglish(t *testing.T) {
	assert.Equal(t, "你好world", joinContextAndText("你好", "world"))
}

func TestJoinContextAndText_EnglishToCJK(t *testing.T) {
	assert.Equal(t, "Hello你好", joinContextAndText("Hello", "你好"))
}

func TestJoinContextAndText_TrailingSpace(t *testing.T) {
	assert.Equal(t, "Hello world", joinContextAndText("Hello ", "world"))
}

func TestJoinContextAndText_TrailingNewline(t *testing.T) {
	assert.Equal(t, "工作计划\n还有托马斯的。", joinContextAndText("工作计划\n", "还有托马斯的。"))
}

func TestJoinContextAndText_TrailingPunctuation(t *testing.T) {
	assert.Equal(t, "Hello,world", joinContextAndText("Hello,", "world"))
}

func TestJoinContextAndText_Hiragana(t *testing.T) {
	assert.Equal(t, "こんにちは世界", joinContextAndText("こんにちは", "世界"))
}

func TestJoinContextAndText_Katakana(t *testing.T) {
	assert.Equal(t, "カタカナテスト", joinContextAndText("カタカナ", "テスト"))
}

func TestJoinContextAndText_Hangul(t *testing.T) {
	assert.Equal(t, "안녕하세요반갑습니다", joinContextAndText("안녕하세요", "반갑습니다"))
}

func TestJoinContextAndText_EmojiContext(t *testing.T) {
	// Emoji is not CJK, not space, not punctuation → add space
	assert.Equal(t, "😀 hello", joinContextAndText("😀", "hello"))
}

func TestJoinContextAndText_CJKPunctuation(t *testing.T) {
	assert.Equal(t, "你好。世界", joinContextAndText("你好。", "世界"))
}

// --- isCJK tests ---

func TestIsCJK(t *testing.T) {
	assert.True(t, isCJK('中'))
	assert.True(t, isCJK('あ'))  // Hiragana
	assert.True(t, isCJK('ア'))  // Katakana
	assert.True(t, isCJK('한'))  // Hangul
	assert.True(t, isCJK('〇'))  // CJK Symbol
	assert.True(t, isCJK('Ａ'))  // Fullwidth
	assert.False(t, isCJK('A'))
	assert.False(t, isCJK('1'))
	assert.False(t, isCJK(' '))
}

// --- isPunctuation tests ---

func TestIsPunctuation(t *testing.T) {
	assert.True(t, isPunctuation('，'))
	assert.True(t, isPunctuation('。'))
	assert.True(t, isPunctuation(','))
	assert.True(t, isPunctuation('.'))
	assert.True(t, isPunctuation('!'))
	assert.True(t, isPunctuation('？'))
	assert.False(t, isPunctuation('A'))
	assert.False(t, isPunctuation('中'))
	assert.False(t, isPunctuation(' '))
}

// --- restoreTrailingWhitespace tests ---

func TestRestoreTrailingWhitespace_NoTrailing(t *testing.T) {
	assert.Equal(t, "modified text", restoreTrailingWhitespace("original", "modified text"))
}

func TestRestoreTrailingWhitespace_AppendScenario(t *testing.T) {
	// LLM preserved original but stripped trailing whitespace
	result := restoreTrailingWhitespace("工作计划\n", "工作计划还有托马斯的。")
	assert.Equal(t, "工作计划\n还有托马斯的。", result)
}

func TestRestoreTrailingWhitespace_AppendWithTrailingSpace(t *testing.T) {
	result := restoreTrailingWhitespace("Hello ", "Hello world")
	assert.Equal(t, "Hello world", result)
}

func TestRestoreTrailingWhitespace_EditScenario(t *testing.T) {
	// LLM modified content, trailing should be appended to end
	result := restoreTrailingWhitespace("工作计划\n", "会议纪要")
	assert.Equal(t, "会议纪要\n", result)
}

func TestRestoreTrailingWhitespace_EditScenarioWithTrailingSpaces(t *testing.T) {
	result := restoreTrailingWhitespace("Hello   ", "Goodbye")
	assert.Equal(t, "Goodbye   ", result)
}

func TestRestoreTrailingWhitespace_EmptyText(t *testing.T) {
	// "Delete everything" scenario
	result := restoreTrailingWhitespace("工作计划\n", "")
	assert.Equal(t, "\n", result)
}

func TestRestoreTrailingWhitespace_EmptyTrimmedContext(t *testing.T) {
	// Context is only whitespace
	result := restoreTrailingWhitespace("  \n", "some text")
	assert.Equal(t, "some text", result)
}

func TestRestoreTrailingWhitespace_TabTrailing(t *testing.T) {
	result := restoreTrailingWhitespace("code\t", "code\tmore")
	// HasPrefix("code\tmore", "code") → true, rest = "\tmore", strip leading \t → "more"
	assert.Equal(t, "code\tmore", result)
}

// --- transcribeAppend tests ---

func TestTranscribeAppend_Gemini_NoContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "准确还原说话内容")
		assert.NotContains(t, prompt, "辅助理解语境")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "transcribed text"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "append"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "transcribed text", text)
	assert.Equal(t, "model-a", model)
}

func TestTranscribeAppend_Gemini_WithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "辅助理解语境和专有名词纠错")
		assert.Contains(t, prompt, "原有文本")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "新的内容"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "append"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "原有文本", "")
	assert.NoError(t, err)
	assert.Equal(t, "原有文本新的内容", text) // CJK join, no space
}

func TestTranscribeAppend_GPT_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/audio/transcriptions", r.URL.Path)

		resp := map[string]string{"text": "GPT transcription"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Engine = "gpt"
	cfg.EditMode = "append"
	cfg.GPTModels = []string{"gpt-a"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.Transcribe([]byte("audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "GPT transcription", text)
	assert.Equal(t, "gpt-a", model)
}

func TestTranscribeAppend_GPT_WithContext_Join(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{"text": "world"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Engine = "gpt"
	cfg.EditMode = "append"
	cfg.GPTModels = []string{"gpt-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "Hello", "")
	assert.NoError(t, err)
	assert.Equal(t, "Hello world", text) // English join with space
}

func TestTranscribeAppend_NoSpeech_Empty_WithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: ""}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "append"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "keep this", "")
	assert.NoError(t, err)
	assert.Equal(t, "keep this", text) // returns contextText on no speech
}

func TestTranscribeAppend_NoSpeech_Sentinel_WithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "[NO_SPEECH]"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "append"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "keep this", "")
	assert.NoError(t, err)
	assert.Equal(t, "keep this", text) // returns contextText on no speech
}

func TestTranscribeAppend_NoSpeech_NoContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "[NO_SPEECH]"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "append"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "", text)
}

// --- transcribeEdit tests ---

func TestTranscribeEdit_NoContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "准确还原说话内容")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "transcribed"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "transcribed", text)
}

func TestTranscribeEdit_AppendScenario(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "工作计划还有托马斯的。"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "工作计划\n", "")
	assert.NoError(t, err)
	assert.Equal(t, "工作计划\n还有托马斯的。", text) // trailing \n restored
}

func TestTranscribeEdit_EditScenario(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "会议纪要"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "工作计划\n", "")
	assert.NoError(t, err)
	assert.Equal(t, "会议纪要\n", text) // trailing \n preserved
}

func TestTranscribeEdit_DeleteEverything(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// LLM returns empty string = "delete everything"
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: ""}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "工作计划\n", "")
	assert.NoError(t, err)
	// Empty string is NOT no-speech in edit mode, it's a valid "delete everything"
	assert.Equal(t, "\n", text) // restoreTrailingWhitespace adds back \n
}

func TestTranscribeEdit_NoSpeech_Sentinel_WithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "[NO_SPEECH]"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "keep this", "")
	assert.NoError(t, err)
	assert.Equal(t, "keep this", text) // returns contextText
}

func TestTranscribeEdit_NoSpeech_Sentinel_NoContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "[NO_SPEECH]"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.EditMode = "edit"
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, _, err := svc.Transcribe([]byte("audio"), "audio/wav", "", "")
	assert.NoError(t, err)
	assert.Equal(t, "", text)
}

// --- callWithModelFallback tests ---

func TestCallWithModelFallback_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.Models = []string{"model-a"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.callWithModelFallback([]byte("audio"), "audio/wav", "prompt")
	assert.NoError(t, err)
	assert.Equal(t, "ok", text)
	assert.Equal(t, "model-a", model)
}

func TestCallWithModelFallback_NoModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl:   "http://unused",
		LiteLLMKey:   "key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{},
	}
	svc := NewVoiceService(cfg)

	_, _, err := svc.callWithModelFallback([]byte("audio"), "audio/wav", "prompt")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no models configured")
}

// --- callGPTWithModelFallback tests ---

func TestCallGPTWithModelFallback_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/audio/transcriptions", r.URL.Path)
		resp := map[string]string{"text": "gpt result"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-a"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.callGPTWithModelFallback([]byte("audio"), "audio/wav", "prompt")
	assert.NoError(t, err)
	assert.Equal(t, "gpt result", text)
	assert.Equal(t, "gpt-a", model)
}

func TestCallGPTWithModelFallback_Fallback(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)
		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "fail"}`))
			return
		}
		resp := map[string]string{"text": "from gpt-b"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := newTestConfig(server.URL)
	cfg.GPTModels = []string{"gpt-a", "gpt-b"}
	svc := NewVoiceService(cfg)

	text, model, err := svc.callGPTWithModelFallback([]byte("audio"), "audio/wav", "prompt")
	assert.NoError(t, err)
	assert.Equal(t, "from gpt-b", text)
	assert.Equal(t, "gpt-b", model)
}

func TestCallGPTWithModelFallback_NoModels(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl:   "http://unused",
		LiteLLMKey:   "key",
		Timeout:      5,
		TotalTimeout: 10,
		GPTModels:    []string{},
	}
	svc := NewVoiceService(cfg)

	_, _, err := svc.callGPTWithModelFallback([]byte("audio"), "audio/wav", "prompt")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no GPT models configured")
}

// --- GPT engine + edit mode rejection ---

func TestTranscribeWithOptions_GPT_EditModeRejected(t *testing.T) {
	cfg := newGPTTestConfig("http://unused.example.com")
	svc := NewVoiceService(cfg)

	// Explicit mode=edit with GPT engine should fail
	_, _, err := svc.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{Mode: "edit"})
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrGPTEditNotSupported)

	// GPT config with default edit mode set to "edit" should also fail
	cfg2 := newGPTTestConfig("http://unused.example.com")
	cfg2.EditMode = "edit"
	svc2 := NewVoiceService(cfg2)

	_, _, err = svc2.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{})
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrGPTEditNotSupported)

	// GPT engine with mode=append should NOT error (sanity check, will fail on network but not on validation)
	_, _, err = svc.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{Mode: "append"})
	assert.Error(t, err) // network error, not validation error
	assert.NotErrorIs(t, err, ErrGPTEditNotSupported)
}

// --- contextText length truncation (MaxContextTextLength) ---

func TestMaxContextTextLength_Constant(t *testing.T) {
	assert.Equal(t, 10000, MaxContextTextLength)
}
