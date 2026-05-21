package bot_api

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
	"github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func buildMultipartRequest(t *testing.T, fields map[string]string, fileName string, fileContent []byte) (*http.Request, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if fileName != "" {
		part, err := w.CreateFormFile("file", fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		part.Write(fileContent)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/bot/voice/transcribe", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, w.FormDataContentType()
}

func TestBotTranscribe_ModeMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		clientMode   string
		expectedMode string
	}{
		{"append maps to append_only", "append", "append_only"},
		{"edit maps to smart", "edit", "smart"},
		{"empty maps to smart", "", "smart"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedMode string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(32 << 20); err != nil {
					t.Fatalf("parse multipart: %v", err)
				}
				receivedMode = r.FormValue("mode")
				body, _ := io.ReadAll(r.Body)
				_ = body
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"status": 200, "text": "hello"})
			}))
			defer srv.Close()

			client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
			ba := &BotAPI{
				Log:          log.NewTLog("BotAPI-test"),
				speechClient: client,
				maxBodySize:  5 << 20,
				maxFileSize:  3 << 20,
			}

			fields := map[string]string{"lang": "en"}
			if tt.clientMode != "" {
				fields["mode"] = tt.clientMode
			}
			httpReq, _ := buildMultipartRequest(t, fields, "audio.wav", []byte("fake-audio"))

			rec := httptest.NewRecorder()
			gc, _ := gin.CreateTestContext(rec)
			gc.Request = httpReq
			c := &wkhttp.Context{Context: gc}
			c.Set(CtxKeyBotKind, BotKindUser)

			ba.botTranscribe(c)

			assert.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())
			assert.Equal(t, tt.expectedMode, receivedMode,
				"mode %q should map to %q, got %q", tt.clientMode, tt.expectedMode, receivedMode)
		})
	}
}

func TestBotTranscribe_ForwardsAudioFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	audioData := []byte("real-audio-content-bytes")
	var receivedFile []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("get form file: %v", err)
		}
		defer f.Close()
		receivedFile, _ = io.ReadAll(f)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": 200})
	}))
	defer srv.Close()

	client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-test"),
		speechClient: client,
		maxBodySize:  5 << 20,
		maxFileSize:  3 << 20,
	}

	httpReq, _ := buildMultipartRequest(t, map[string]string{"mode": "edit"}, "audio.wav", audioData)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.botTranscribe(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, audioData, receivedFile, "audio file content must be forwarded intact")
}

func TestBotTranscribe_AppBotRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ba := &BotAPI{Log: log.NewTLog("BotAPI-test")}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/voice/transcribe", nil)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindApp)

	ba.botTranscribe(c)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestBotPutVoiceContext_EmptyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-test"),
		spaceQuerier:          q,
		maxVoiceContextLength: 10000,
	}

	body, _ := json.Marshal(map[string]string{"context": ""})
	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botPutVoiceContext(c)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBotPutVoiceContext_WhitespaceOnlyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-test"),
		spaceQuerier:          q,
		maxVoiceContextLength: 10000,
	}

	body, _ := json.Marshal(map[string]string{"context": "   \t\n  "})
	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botPutVoiceContext(c)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestBotPutVoiceContext_Overlength(t *testing.T) {
	gin.SetMode(gin.TestMode)

	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-test"),
		spaceQuerier:          q,
		maxVoiceContextLength: 10,
	}

	body, _ := json.Marshal(map[string]string{"context": "this is way too long for the limit"})
	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botPutVoiceContext(c)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "max length")
}

func TestResolveOwnerAndSpace_AppBotRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ba := &BotAPI{Log: log.NewTLog("BotAPI-test")}

	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", nil)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindApp)

	_, _, _, ok := ba.resolveOwnerAndSpace(c)

	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "app bot does not support voice")
}

func TestResolveOwnerAndSpace_NilRobot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ba := &BotAPI{Log: log.NewTLog("BotAPI-test")}

	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", nil)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)

	_, _, _, ok := ba.resolveOwnerAndSpace(c)

	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestBotPutVoiceContext_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": 200, "msg": "ok"})
	}))
	defer srv.Close()

	client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:                   log.NewTLog("BotAPI-test"),
		spaceQuerier:          q,
		speechClient:          client,
		maxVoiceContextLength: 10000,
	}

	body, _ := json.Marshal(map[string]string{"context": "hello world"})
	httpReq := httptest.NewRequest(http.MethodPut, "/v1/bot/voice/context", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botPutVoiceContext(c)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBotGetVoiceContext_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(voice_adapter.VocabularyResponse{
			HasContent: true,
			Content:    "test vocab",
			UpdatedAt:  "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-test"),
		spaceQuerier: q,
		speechClient: client,
	}

	httpReq := httptest.NewRequest(http.MethodGet, "/v1/bot/voice/context", nil)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botGetVoiceContext(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	assert.Equal(t, true, resp["has_context"])
	assert.Equal(t, "test vocab", resp["context"])
}

func TestBotDeleteVoiceContext_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": 200, "msg": "ok"})
	}))
	defer srv.Close()

	client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	q := &fakeSpaceQuerier{defaultSpace: "space_A"}
	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-test"),
		spaceQuerier: q,
		speechClient: client,
	}

	httpReq := httptest.NewRequest(http.MethodDelete, "/v1/bot/voice/context", nil)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)
	c.Set(CtxKeyRobot, &robotModel{RobotID: "bot_1", CreatorUID: "user_1"})

	ba.botDeleteVoiceContext(c)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBotTranscribe_UnknownModeRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-test"),
		speechClient: voice_adapter.NewSpeechClient("http://unused", "test-key", 5*time.Second),
		maxBodySize:  5 << 20,
		maxFileSize:  3 << 20,
	}

	httpReq, _ := buildMultipartRequest(t, map[string]string{"mode": "custom_mode"}, "audio.wav", []byte("data"))
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.botTranscribe(c)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid mode")
}

func TestBotTranscribe_MultipleFormFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	receivedFields := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		for k, vals := range r.MultipartForm.Value {
			receivedFields[k] = vals[0]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": 200})
	}))
	defer srv.Close()

	client := voice_adapter.NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	ba := &BotAPI{
		Log:          log.NewTLog("BotAPI-test"),
		speechClient: client,
		maxBodySize:  5 << 20,
		maxFileSize:  3 << 20,
	}

	fields := map[string]string{
		"mode": "append",
		"lang": "zh",
		"hint": "technical terms",
	}
	httpReq, _ := buildMultipartRequest(t, fields, "audio.wav", []byte("data"))
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.botTranscribe(c)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "append_only", receivedFields["mode"])
	assert.Equal(t, "zh", receivedFields["lang"])
	assert.Equal(t, "technical terms", receivedFields["hint"])
}

func TestBotTranscribe_InvalidMultipartForm(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ba := &BotAPI{
		Log:         log.NewTLog("BotAPI-test"),
		maxBodySize: 5 << 20,
		maxFileSize: 3 << 20,
	}

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/voice/transcribe",
		strings.NewReader("not a multipart form"))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.botTranscribe(c)

	assert.NotEqual(t, http.StatusOK, rec.Code)
}
