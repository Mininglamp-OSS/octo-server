package voice_adapter

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
