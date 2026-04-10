package botfather

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-server/modules/voice"
	"github.com/stretchr/testify/assert"
)

// setupVoiceTestEnv creates a test server with a bot, owner, space, and membership.
// Returns the server, BotFather, and bot token for API auth.
func setupVoiceTestEnv(t *testing.T) (*server.Server, *BotFather, string) {
	s, bf := setupTestBotFather(t)

	ownerUID := "voice_owner"
	robotID := "voice_bot"
	spaceID := "voice_space"
	botToken := "bf_" + robotID

	createTestUser(t, bf, ownerUID, "Voice Owner")
	createTestRobot(t, bf, robotID, ownerUID, 0)

	// Create space
	_, err := bf.db.session.InsertBySql(
		"INSERT INTO space (space_id, name, status) VALUES (?, ?, 1)",
		spaceID, "Test Space",
	).Exec()
	assert.NoError(t, err)

	// Add robot to space
	_, err = bf.db.session.InsertBySql(
		"INSERT INTO space_member (space_id, uid, status) VALUES (?, ?, 1)",
		spaceID, robotID,
	).Exec()
	assert.NoError(t, err)

	return s, bf, botToken
}

// --- PUT /v1/bot/voice/context tests ---

func TestBotPutVoiceContext_Success(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	body, _ := json.Marshal(map[string]string{"context": "test correction context"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, float64(200), resp["status"])
	assert.Equal(t, "ok", resp["msg"])
}

func TestBotPutVoiceContext_EmptyContext(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	body, _ := json.Marshal(map[string]string{"context": ""})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "context cannot be empty")
}

func TestBotPutVoiceContext_WhitespaceOnlyContext(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	body, _ := json.Marshal(map[string]string{"context": "   \n\t  "})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "context cannot be empty")
}

func TestBotPutVoiceContext_ExceedsMaxLength(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	longContext := strings.Repeat("a", voice.MaxVoiceContextLength+1)
	body, _ := json.Marshal(map[string]string{"context": longContext})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "context exceeds max length")
}

func TestBotPutVoiceContext_ExactMaxLength(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	exactContext := strings.Repeat("a", voice.MaxVoiceContextLength)
	body, _ := json.Marshal(map[string]string{"context": exactContext})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// --- GET /v1/bot/voice/context tests ---

func TestBotGetVoiceContext_NotSet(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, false, resp["has_context"])
	assert.Equal(t, "", resp["context"])
	assert.Equal(t, "", resp["updated_at"])
}

func TestBotGetVoiceContext_AfterPut(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	// PUT context first
	body, _ := json.Marshal(map[string]string{"context": "my correction context"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET context
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, true, resp["has_context"])
	assert.Equal(t, "my correction context", resp["context"])
	assert.NotEmpty(t, resp["updated_at"])
}

// --- DELETE /v1/bot/voice/context tests ---

func TestBotDeleteVoiceContext_Success(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	// PUT first
	body, _ := json.Marshal(map[string]string{"context": "to be deleted"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// DELETE
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "ok", resp["msg"])

	// GET should show no context
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, false, resp["has_context"])
}

func TestBotDeleteVoiceContext_NotExists(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code) // idempotent
}

// --- POST /v1/bot/voice/transcribe tests ---

func TestBotTranscribe_InvalidMode(t *testing.T) {
	s, bf, botToken := setupVoiceTestEnv(t)
	bf.voiceCfg.LiteLLMUrl = "https://unused.example.com"
	bf.voiceCfg.LiteLLMKey = "key"

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("audio", "test.wav")
	part.Write([]byte("fake-audio"))
	writer.WriteField("mode", "invalid")
	writer.Close()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/voice/transcribe", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "mode must be 'append' or 'edit'")
}

func TestBotTranscribe_MissingAudio(t *testing.T) {
	s, bf, botToken := setupVoiceTestEnv(t)
	bf.voiceCfg.LiteLLMUrl = "https://unused.example.com"
	bf.voiceCfg.LiteLLMKey = "key"

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/voice/transcribe", nil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "audio file is required")
}

func TestBotTranscribe_FileSizeExceeded(t *testing.T) {
	s, bf, botToken := setupVoiceTestEnv(t)
	bf.voiceCfg.MaxFileSize = 100
	bf.voiceCfg.LiteLLMUrl = "https://unused.example.com"
	bf.voiceCfg.LiteLLMKey = "key"

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("audio", "test.wav")
	part.Write(make([]byte, 200))
	writer.Close()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/voice/transcribe", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "file size exceeds limit")
}

func TestBotTranscribe_GPT_EditModeRejected(t *testing.T) {
	s, bf, botToken := setupVoiceTestEnv(t)
	bf.voiceCfg.LiteLLMUrl = "https://unused.example.com"
	bf.voiceCfg.LiteLLMKey = "key"
	bf.voiceCfg.Engine = "gpt"
	bf.voiceCfg.EditMode = "append"

	// Explicit mode=edit with GPT engine should return 400
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("audio", "test.wav")
	part.Write([]byte("fake-audio"))
	writer.WriteField("mode", "edit")
	writer.Close()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/voice/transcribe", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "edit mode is not supported with GPT engine")
}

func TestBotTranscribe_GPT_EditModeRejected_DefaultMode(t *testing.T) {
	s, bf, botToken := setupVoiceTestEnv(t)
	bf.voiceCfg.LiteLLMUrl = "https://unused.example.com"
	bf.voiceCfg.LiteLLMKey = "key"
	bf.voiceCfg.Engine = "gpt"
	bf.voiceCfg.EditMode = "edit" // default mode is edit

	// No explicit mode field, but default is edit → should return 400
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("audio", "test.wav")
	part.Write([]byte("fake-audio"))
	writer.Close()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/bot/voice/transcribe", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "edit mode is not supported with GPT engine")
}

// --- Auth tests ---

func TestBotVoiceContext_InvalidToken(t *testing.T) {
	s, _ := setupTestBotFather(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer invalid_token")
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// --- Upsert idempotency test ---

func TestBotPutVoiceContext_Upsert(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	// First PUT
	body, _ := json.Marshal(map[string]string{"context": "first context"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Second PUT (upsert)
	body, _ = json.Marshal(map[string]string{"context": "updated context"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// GET should return updated context
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/v1/bot/voice/context", nil)
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "updated context", resp["context"])
}

// --- CJK context length test ---

func TestBotPutVoiceContext_CJKRuneCount(t *testing.T) {
	s, _, botToken := setupVoiceTestEnv(t)

	// Exactly MaxVoiceContextLength CJK characters (each is 3 bytes but 1 rune)
	exactContext := strings.Repeat("你", voice.MaxVoiceContextLength)
	body, _ := json.Marshal(map[string]string{"context": exactContext})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// One more CJK character should exceed limit
	overContext := strings.Repeat("你", voice.MaxVoiceContextLength+1)
	body, _ = json.Marshal(map[string]string{"context": overContext})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/v1/bot/voice/context", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+botToken)
	s.GetRoute().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
