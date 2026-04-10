package voice

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// mockVoiceStore is a mock implementation of VoiceStore for testing
type mockVoiceStore struct {
	contextResult    *UserVoiceContextModel
	contextErr       error
	membershipResult bool
	membershipErr    error
}

func (m *mockVoiceStore) QueryVoiceContext(uid, spaceID string) (*UserVoiceContextModel, error) {
	return m.contextResult, m.contextErr
}

func (m *mockVoiceStore) CheckSpaceMembership(spaceID, uid string) (bool, error) {
	return m.membershipResult, m.membershipErr
}

// setupTestRouter creates a test router with voice endpoints (no auth middleware)
func setupTestRouter(cfg *VoiceConfig, litellmURL string) *wkhttp.WKHttp {
	if litellmURL != "" {
		cfg.LiteLLMUrl = litellmURL
	}

	svc := NewVoiceService(cfg)
	r := wkhttp.New()

	v := &Voice{cfg: cfg, service: svc, Log: log.NewTLog("VoiceTest")}

	// Register routes without auth middleware for testing
	group := r.Group("/v1/voice")
	{
		group.POST("/transcribe", v.transcribe)
		group.GET("/config", v.getConfig)
	}

	return r
}

type multipartOpts struct {
	contextText string
	chatContext string
}

func createMultipartRequest(t *testing.T, path string, audioData []byte, contextText string) *http.Request {
	return createMultipartRequestWithOpts(t, path, audioData, multipartOpts{contextText: contextText})
}

func createMultipartRequestWithOpts(t *testing.T, path string, audioData []byte, opts multipartOpts) *http.Request {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("audio", "test.wav")
	assert.NoError(t, err)
	_, err = part.Write(audioData)
	assert.NoError(t, err)

	if opts.contextText != "" {
		err = writer.WriteField("context_text", opts.contextText)
		assert.NoError(t, err)
	}

	if opts.chatContext != "" {
		err = writer.WriteField("chat_context", opts.chatContext)
		assert.NoError(t, err)
	}

	err = writer.Close()
	assert.NoError(t, err)

	req, err := http.NewRequest("POST", path, body)
	assert.NoError(t, err)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func newTestAPIConfig(litellmURL string) *VoiceConfig {
	return &VoiceConfig{
		LiteLLMUrl:   litellmURL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"test-model"},
		GPTModels:    []string{"gpt-test"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		Engine:       "gemini",
		EditMode:     "edit",
	}
}

func TestTranscribeAPI_Success(t *testing.T) {
	// Mock LiteLLM server
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Transcribed text"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio-data"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NoError(t, err)
	assert.Equal(t, float64(200), resp["status"])
	assert.Equal(t, "Transcribed text", resp["text"])
	assert.Equal(t, "test-model", resp["m"])
}

func TestTranscribeAPI_WithContextText(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		// In edit mode, prompt uses modifyPromptTemplate
		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "已有以下文本")
		assert.Contains(t, prompt, "my existing content")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Modified content"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio-data"), "my existing content")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "Modified content", resp["text"])
}

func TestTranscribeAPI_MissingAudioFile(t *testing.T) {
	cfg := newTestAPIConfig("https://unused.example.com")
	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/voice/transcribe", nil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "audio file is required")
}

func TestTranscribeAPI_FileSizeExceeded(t *testing.T) {
	cfg := newTestAPIConfig("https://unused.example.com")
	cfg.MaxFileSize = 100 // Very small limit
	router := setupTestRouter(cfg, "")

	// Create a file larger than 100 bytes
	largeData := make([]byte, 200)
	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", largeData, "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "file size exceeds limit")
}

func TestTranscribeAPI_ServerError(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "bad request"}`))
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "transcription failed")
}

func TestGetConfigAPI_Enabled(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl:   "https://example.com",
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"test-model"},
		MaxDuration:  90,
		MaxFileSize:  5 * 1024 * 1024,
		Engine:       "gemini",
		EditMode:     "edit",
	}

	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/config", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, true, resp["enabled"])
	assert.Equal(t, float64(90), resp["max_duration"])
	assert.Equal(t, "gm", resp["engine"])
	assert.Equal(t, "edit", resp["edit_mode"])
}

func TestGetConfigAPI_Disabled(t *testing.T) {
	cfg := &VoiceConfig{
		// Missing URL and Key - validation will fail
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"test-model"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		Engine:       "gemini",
		EditMode:     "edit",
	}

	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/config", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, false, resp["enabled"])
	assert.Equal(t, float64(60), resp["max_duration"])
}

func TestTranscribeAPI_AudioRequestFormat(t *testing.T) {
	// Verify the request sent to LiteLLM has correct structure
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatCompletionRequest
		err := json.Unmarshal(body, &req)
		assert.NoError(t, err)

		assert.Equal(t, "test-model", req.Model)
		assert.Len(t, req.Messages, 1)
		assert.Equal(t, "user", req.Messages[0].Role)
		assert.Len(t, req.Messages[0].Content, 2)
		assert.Equal(t, "text", req.Messages[0].Content[0].Type)
		assert.Equal(t, "input_audio", req.Messages[0].Content[1].Type)
		assert.NotEmpty(t, req.Messages[0].Content[1].InputAudio.Data)

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "OK"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("test-audio-bytes"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTranscribeAPI_WithChatContext(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.Contains(t, prompt, "辅助识别专有名词拼写")
		assert.Contains(t, prompt, "Alice: 明天开会")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "OK with chat context"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	req := createMultipartRequestWithOpts(t, "/v1/voice/transcribe", []byte("fake-audio"), multipartOpts{
		chatContext: "Alice: 明天开会",
	})
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "OK with chat context", resp["text"])
}

func TestTranscribeAPI_ChatContextTruncation(t *testing.T) {
	var receivedPrompt string
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedPrompt = req.Messages[0].Content[0].Text

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "OK"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)

	// Create chat context that exceeds MaxChatContextLength
	longPrefix := strings.Repeat("A", 5000) // will be truncated away
	longSuffix := strings.Repeat("B", MaxChatContextLength)
	longChatContext := longPrefix + longSuffix
	assert.True(t, len(longChatContext) > MaxChatContextLength)

	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	req := createMultipartRequestWithOpts(t, "/v1/voice/transcribe", []byte("fake-audio"), multipartOpts{
		chatContext: longChatContext,
	})
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// The truncated context should contain only the suffix (last MaxChatContextLength chars)
	assert.Contains(t, receivedPrompt, longSuffix)
	assert.NotContains(t, receivedPrompt, longPrefix)
}

func TestTranscribeAPI_EmptyChatContext(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)

		prompt := req.Messages[0].Content[0].Text
		assert.NotContains(t, prompt, "辅助识别专有名词拼写")

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "plain transcription"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	// No chat_context field at all
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "plain transcription", resp["text"])
}

// --- GPT engine API tests ---

func TestTranscribeAPI_GPTEngine(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/audio/transcriptions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "GPT transcribed"})
	}))
	defer litellmServer.Close()

	cfg := &VoiceConfig{
		LiteLLMUrl:   litellmServer.URL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Engine:       "gpt",
		GPTModels:    []string{"gpt-4o-mini-transcribe"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		EditMode:     "append",
	}

	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "GPT transcribed", resp["text"])
	assert.Equal(t, "gpt4omt", resp["m"])
	assert.Equal(t, "gp", resp["engine"])
}

func TestTranscribeAPI_GeminiEngineShortened(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "Gemini text"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := &VoiceConfig{
		LiteLLMUrl:   litellmServer.URL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Engine:       "gemini",
		Models:       []string{"gemini-3-flash-preview"},
		MaxDuration:  60,
		MaxFileSize:  5 * 1024 * 1024,
		EditMode:     "edit",
	}

	router := setupTestRouter(cfg, "")

	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio"), "")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "gm", resp["engine"])
}

// --- contextText truncation tests ---

func TestTranscribeAPI_ContextTextTruncation(t *testing.T) {
	var receivedPrompt string
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedPrompt = req.Messages[0].Content[0].Text

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "OK"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)

	longPrefix := strings.Repeat("X", 5000)
	longSuffix := strings.Repeat("Y", MaxContextTextLength)
	longContextText := longPrefix + longSuffix

	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	req := createMultipartRequest(t, "/v1/voice/transcribe", []byte("fake-audio"), longContextText)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// Should keep the tail (recent text), truncate the head
	assert.Contains(t, receivedPrompt, longSuffix)
	assert.NotContains(t, receivedPrompt, longPrefix)
}

// --- getConfig edit_mode and engine tests ---

func TestGetConfigAPI_EditModeAndEngine(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl:  "https://example.com",
		LiteLLMKey:  "key",
		Models:      []string{"m"},
		GPTModels:   []string{"gpt-m"},
		MaxDuration: 60,
		MaxFileSize: 5 * 1024 * 1024,
		Engine:      "gpt",
		EditMode:    "append",
	}

	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/config", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "gp", resp["engine"])
	assert.Equal(t, "append", resp["edit_mode"])
}

func TestShortenModelName(t *testing.T) {
	assert.Equal(t, "gpt4omt", shortenModelName("gpt-4o-mini-transcribe"))
	assert.Equal(t, "g31pp", shortenModelName("gemini-3.1-pro-preview"))
	assert.Equal(t, "g3fp", shortenModelName("gemini-3-flash-preview"))
	assert.Equal(t, "g25p", shortenModelName("gemini-2.5-pro"))
	assert.Equal(t, "unknown-model", shortenModelName("unknown-model"))
}

func TestShortenEngineName(t *testing.T) {
	assert.Equal(t, "gm", shortenEngineName("gemini"))
	assert.Equal(t, "gp", shortenEngineName("gpt"))
	assert.Equal(t, "other", shortenEngineName("other"))
}

// --- getConfig max_file_size tests ---

func TestGetConfigAPI_MaxFileSize(t *testing.T) {
	cfg := &VoiceConfig{
		LiteLLMUrl:  "https://example.com",
		LiteLLMKey:  "key",
		Models:      []string{"m"},
		MaxDuration: 60,
		MaxFileSize: 3 * 1024 * 1024,
		Engine:      "gemini",
		EditMode:    "edit",
	}

	router := setupTestRouter(cfg, "")
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/config", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, float64(3*1024*1024), resp["max_file_size"])
	assert.Equal(t, true, resp["enabled"])
}

// --- TranscribeWithOptions tests ---

func TestTranscribeWithOptions_ModeOverride(t *testing.T) {
	// Edit mode: prompt includes "已有以下文本"
	// Append mode: prompt includes append template context hint
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "result text"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := &VoiceConfig{
		LiteLLMUrl:   litellmServer.URL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"test-model"},
		Engine:       "gemini",
		EditMode:     "edit", // global default is edit
		MaxFileSize:  3 * 1024 * 1024,
	}
	svc := NewVoiceService(cfg)

	// Override to append mode
	text, model, err := svc.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{Mode: "append"})
	assert.NoError(t, err)
	assert.Equal(t, "result text", text)
	assert.Equal(t, "test-model", model)
}

func TestTranscribeWithOptions_EmptyOptionsFallback(t *testing.T) {
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "transcribed"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := &VoiceConfig{
		LiteLLMUrl:   litellmServer.URL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"test-model"},
		Engine:       "gemini",
		EditMode:     "edit",
		MaxFileSize:  3 * 1024 * 1024,
	}
	svc := NewVoiceService(cfg)

	// Empty options should fall back to global config
	text, _, err := svc.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "transcribed", text)
}

func TestTranscribeWithOptions_ModelOverride(t *testing.T) {
	var requestedModel string
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		requestedModel = req.Model

		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "ok"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := &VoiceConfig{
		LiteLLMUrl:   litellmServer.URL,
		LiteLLMKey:   "test-key",
		Timeout:      5,
		TotalTimeout: 10,
		Models:       []string{"default-model"},
		Engine:       "gemini",
		EditMode:     "edit",
		MaxFileSize:  3 * 1024 * 1024,
	}
	svc := NewVoiceService(cfg)

	// Override model
	_, usedModel, err := svc.TranscribeWithOptions([]byte("audio"), "audio/wav", "", "", TranscribeOptions{Model: "custom-model"})
	assert.NoError(t, err)
	assert.Equal(t, "custom-model", requestedModel)
	assert.Equal(t, "custom-model", usedModel)
}

// --- Rune-safe context truncation in transcribe handler ---

// --- GET /v1/voice/context tests (mocked DB) ---

func setupContextTestRouter(store VoiceStore) *wkhttp.WKHttp {
	r := wkhttp.New()
	v := &Voice{
		cfg: &VoiceConfig{},
		db:  store,
		Log: log.NewTLog("VoiceTest"),
	}

	// Fake auth middleware that sets uid
	group := r.Group("/v1/voice", func(c *wkhttp.Context) {
		c.Set("uid", "test_user")
		c.Next()
	})
	group.GET("/context", v.getContext)
	return r
}

func TestGetContextAPI_HasContext(t *testing.T) {
	store := &mockVoiceStore{
		membershipResult: true,
		contextResult: &UserVoiceContextModel{
			UID:               "test_user",
			SpaceID:           "space1",
			ASRCorrectContext: "my correction terms",
			UpdatedAt:         time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
		},
	}
	router := setupContextTestRouter(store)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/context?space_id=space1", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, true, resp["has_context"])
	assert.Equal(t, "my correction terms", resp["context"])
	assert.NotEmpty(t, resp["updated_at"])
}

func TestGetContextAPI_NoContext(t *testing.T) {
	store := &mockVoiceStore{
		membershipResult: true,
		contextResult:    nil,
	}
	router := setupContextTestRouter(store)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/context?space_id=space1", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, false, resp["has_context"])
	assert.Equal(t, "", resp["context"])
}

func TestGetContextAPI_MissingSpaceID(t *testing.T) {
	store := &mockVoiceStore{}
	router := setupContextTestRouter(store)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/context", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "space_id is required")
}

func TestGetContextAPI_NotMember(t *testing.T) {
	store := &mockVoiceStore{
		membershipResult: false,
	}
	router := setupContextTestRouter(store)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/voice/context?space_id=space1", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "no permission")
}

func TestTranscribeAPI_RuneSafeChatContextTruncation(t *testing.T) {
	var receivedPrompt string
	litellmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedPrompt = req.Messages[0].Content[0].Text
		resp := chatCompletionResponse{
			Choices: []choice{{Message: responseMessage{Content: "OK"}}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer litellmServer.Close()

	cfg := newTestAPIConfig(litellmServer.URL)
	router := setupTestRouter(cfg, "")

	// CJK characters: each is 1 rune but 3 bytes
	cjkTail := strings.Repeat("你", MaxChatContextLength)
	longChatContext := "AAA" + cjkTail // 3 extra ASCII chars
	assert.True(t, len([]rune(longChatContext)) > MaxChatContextLength)

	w := httptest.NewRecorder()
	req := createMultipartRequestWithOpts(t, "/v1/voice/transcribe", []byte("fake-audio"), multipartOpts{
		chatContext: longChatContext,
	})
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// Truncation should keep the tail (CJK chars), not break multi-byte chars
	assert.Contains(t, receivedPrompt, cjkTail)
	assert.NotContains(t, receivedPrompt, "AAA")
}
