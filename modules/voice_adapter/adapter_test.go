package voice_adapter

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

func TestNewAdapterConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("SPEECH_SERVICE_URL", "")
	t.Setenv("SPEECH_API_KEY", "")
	t.Setenv("SPEECH_TIMEOUT", "")
	t.Setenv("SPEECH_MAX_BODY_SIZE", "")

	cfg := NewAdapterConfigFromEnv()

	if cfg.SpeechTimeout != 50*time.Second {
		t.Errorf("expected default timeout 50s, got %v", cfg.SpeechTimeout)
	}
}

func TestNewAdapterConfigFromEnv_Custom(t *testing.T) {
	t.Setenv("SPEECH_SERVICE_URL", "http://speech:8780")
	t.Setenv("SPEECH_API_KEY", "my-key")
	t.Setenv("SPEECH_TIMEOUT", "30")

	cfg := NewAdapterConfigFromEnv()

	if cfg.SpeechServiceURL != "http://speech:8780" {
		t.Errorf("unexpected URL: %s", cfg.SpeechServiceURL)
	}
	if cfg.SpeechAPIKey != "my-key" {
		t.Errorf("unexpected key: %s", cfg.SpeechAPIKey)
	}
	if cfg.SpeechTimeout != 30*time.Second {
		t.Errorf("expected 30s, got %v", cfg.SpeechTimeout)
	}
}

func TestNewAdapterConfigFromEnv_InvalidValues(t *testing.T) {
	t.Setenv("SPEECH_TIMEOUT", "invalid")

	cfg := NewAdapterConfigFromEnv()

	if cfg.SpeechTimeout != 50*time.Second {
		t.Errorf("expected default timeout 50s for invalid value, got %v", cfg.SpeechTimeout)
	}
}

func newTestAdapter(speechURL string) *VoiceAdapter {
	return &VoiceAdapter{
		client: NewSpeechClient(speechURL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
}

func callGetConfig(a *VoiceAdapter) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	ctx := &wkhttp.Context{Context: gc}
	a.getConfig(ctx)
	return rec
}

func TestGetConfigHandler_Healthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":      true,
			"max_duration": 60,
		})
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
	if body["max_duration"] != float64(60) {
		t.Errorf("expected max_duration=60, got %v", body["max_duration"])
	}
}

func TestGetConfigHandler_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	a := newTestAdapter(srv.URL)
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (graceful fallback), got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("expected enabled=false for fallback, got %v", body["enabled"])
	}
}

func TestGetConfigHandler_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	a := newTestAdapter("http://" + addr)
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (graceful fallback), got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("expected enabled=false for connection refused, got %v", body["enabled"])
	}
}

func TestGetConfigHandler_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := &VoiceAdapter{
		client: NewSpeechClient(srv.URL, "test-key", 100*time.Millisecond),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (graceful fallback), got %d", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("expected enabled=false for timeout, got %v", body["enabled"])
	}
}

func TestNewAdapterConfigFromEnv_MaxBodySize(t *testing.T) {
	t.Setenv("SPEECH_MAX_BODY_SIZE", "1048576")
	t.Setenv("SPEECH_SERVICE_URL", "")
	t.Setenv("SPEECH_API_KEY", "")
	t.Setenv("SPEECH_TIMEOUT", "")

	cfg := NewAdapterConfigFromEnv()

	if cfg.MaxBodySize != 1048576 {
		t.Errorf("expected MaxBodySize 1048576, got %d", cfg.MaxBodySize)
	}
}

func TestNewAdapterConfigFromEnv_MaxBodySizeDefault(t *testing.T) {
	t.Setenv("SPEECH_MAX_BODY_SIZE", "")
	t.Setenv("SPEECH_SERVICE_URL", "")
	t.Setenv("SPEECH_API_KEY", "")
	t.Setenv("SPEECH_TIMEOUT", "")

	cfg := NewAdapterConfigFromEnv()

	if cfg.MaxBodySize != 5<<20 {
		t.Errorf("expected default MaxBodySize 5MB, got %d", cfg.MaxBodySize)
	}
}

func TestGetConfigHandler_InjectsFeedbackPrivacyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
		})
	}))
	defer srv.Close()

	a := &VoiceAdapter{
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{FeedbackPrivacyURL: "https://example.com/privacy"},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["feedback_privacy_url"] != "https://example.com/privacy" {
		t.Errorf("expected feedback_privacy_url='https://example.com/privacy', got %v", body["feedback_privacy_url"])
	}
	if body["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", body["enabled"])
	}
}

func TestGetConfigHandler_NoFeedbackPrivacyURLWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
		})
	}))
	defer srv.Close()

	a := &VoiceAdapter{
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{FeedbackPrivacyURL: ""},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, exists := body["feedback_privacy_url"]; exists {
		t.Errorf("expected no feedback_privacy_url key when empty, but got %v", body["feedback_privacy_url"])
	}
}

func TestNewAdapterConfigFromEnv_FeedbackPrivacyURL(t *testing.T) {
	t.Setenv("VOICE_FEEDBACK_PRIVACY_URL", "https://example.com/policy")
	t.Setenv("SPEECH_SERVICE_URL", "")
	t.Setenv("SPEECH_API_KEY", "")
	t.Setenv("SPEECH_TIMEOUT", "")
	t.Setenv("SPEECH_MAX_BODY_SIZE", "")

	cfg := NewAdapterConfigFromEnv()

	if cfg.FeedbackPrivacyURL != "https://example.com/policy" {
		t.Errorf("expected FeedbackPrivacyURL='https://example.com/policy', got %q", cfg.FeedbackPrivacyURL)
	}
}

func TestNewAdapterConfigFromEnv_NewURLFields(t *testing.T) {
	t.Setenv("VOICE_FEEDBACK_USER_AGREEMENT_URL", "https://example.com/agreement")
	t.Setenv("VOICE_ASR_SERVICE_DOC_FILE", "/tmp/doc.html")
	t.Setenv("SPEECH_SERVICE_URL", "")
	t.Setenv("SPEECH_API_KEY", "")
	t.Setenv("SPEECH_TIMEOUT", "")
	t.Setenv("SPEECH_MAX_BODY_SIZE", "")
	t.Setenv("VOICE_FEEDBACK_PRIVACY_URL", "")

	cfg := NewAdapterConfigFromEnv()

	if cfg.UserAgreementURL != "https://example.com/agreement" {
		t.Errorf("expected UserAgreementURL='https://example.com/agreement', got %q", cfg.UserAgreementURL)
	}
	if cfg.ASRServiceDocFile != "/tmp/doc.html" {
		t.Errorf("expected ASRServiceDocFile='/tmp/doc.html', got %q", cfg.ASRServiceDocFile)
	}
}

func TestGetConfigHandler_InjectsNewURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
		})
	}))
	defer srv.Close()

	a := &VoiceAdapter{
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg: &AdapterConfig{
			FeedbackPrivacyURL: "https://example.com/privacy",
			UserAgreementURL:   "https://example.com/agreement",
		},
		Log: log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["feedback_privacy_url"] != "https://example.com/privacy" {
		t.Errorf("expected feedback_privacy_url='https://example.com/privacy', got %v", body["feedback_privacy_url"])
	}
	if body["feedback_user_agreement_url"] != "https://example.com/agreement" {
		t.Errorf("expected feedback_user_agreement_url='https://example.com/agreement', got %v", body["feedback_user_agreement_url"])
	}
}

func TestGetConfigHandler_NoNewURLsWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled": true,
		})
	}))
	defer srv.Close()

	a := &VoiceAdapter{
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, exists := body["feedback_user_agreement_url"]; exists {
		t.Errorf("expected no feedback_user_agreement_url key when empty")
	}
}

func callGetDocument(a *VoiceAdapter) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/document/asr_service_doc", nil)
	ctx := &wkhttp.Context{Context: gc}
	a.getDocument(ctx)
	return rec
}

func TestGetDocument_ASRServiceDoc(t *testing.T) {
	tmpDir := t.TempDir()
	docPath := filepath.Join(tmpDir, "asr_service_doc.html")
	if err := os.WriteFile(docPath, []byte("<div>test content</div>"), 0644); err != nil {
		t.Fatalf("write test doc: %v", err)
	}

	a := &VoiceAdapter{
		cfg: &AdapterConfig{ASRServiceDocFile: docPath},
		Log: log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetDocument(a)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["doc_type"] != "asr_service_doc" {
		t.Errorf("expected doc_type='asr_service_doc', got %v", body["doc_type"])
	}
	if body["title"] != "Octo 语音转写服务说明" {
		t.Errorf("expected title='Octo 语音转写服务说明', got %v", body["title"])
	}
	if body["content"] != "<div>test content</div>" {
		t.Errorf("expected content='<div>test content</div>', got %v", body["content"])
	}
	if body["version"] != "2.0" {
		t.Errorf("expected version='2.0', got %v", body["version"])
	}
}

func TestGetDocument_NotFound(t *testing.T) {
	a := &VoiceAdapter{
		cfg: &AdapterConfig{ASRServiceDocFile: "/nonexistent/path/doc.html"},
		Log: log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetDocument(a)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "document not available" {
		t.Errorf("expected msg='document not available', got %v", body["msg"])
	}
}
