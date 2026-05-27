package voice_adapter

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
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

func newTestAdapterWithDB(speechURL string, ctx *config.Context) *VoiceAdapter {
	return &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(speechURL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
}

func newMockedDBSession(t *testing.T) (*dbr.Session, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)
	return session, mock, func() { _ = rawDB.Close() }
}

func injectMockDBIntoContext(t *testing.T, ctx *config.Context, session *dbr.Session) {
	t.Helper()
	ctxVal := reflect.ValueOf(ctx).Elem()

	onceField := ctxVal.FieldByName("mysqlOnce")
	once := (*sync.Once)(unsafe.Pointer(onceField.UnsafeAddr()))
	once.Do(func() {})

	sessionField := ctxVal.FieldByName("mySQLSession")
	reflect.NewAt(sessionField.Type(), unsafe.Pointer(sessionField.UnsafeAddr())).
		Elem().Set(reflect.ValueOf(session))
}

func newTestContextWithMockDB(t *testing.T) (*config.Context, sqlmock.Sqlmock, func()) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)

	session, mock, cleanup := newMockedDBSession(t)
	injectMockDBIntoContext(t, ctx, session)
	return ctx, mock, cleanup
}

func callGetConfig(a *VoiceAdapter, mock sqlmock.Sqlmock) *httptest.ResponseRecorder {
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	gc.Request.Header.Set("X-Space-ID", "test-space")
	gc.Set("uid", "test-user")
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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := newTestAdapterWithDB(srv.URL, ctx)
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := newTestAdapterWithDB(srv.URL, ctx)
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := newTestAdapterWithDB("http://"+addr, ctx)
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(srv.URL, "test-key", 100*time.Millisecond),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{FeedbackPrivacyURL: "https://example.com/privacy"},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{FeedbackPrivacyURL: ""},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg: &AdapterConfig{
			FeedbackPrivacyURL: "https://example.com/privacy",
			UserAgreementURL:   "https://example.com/agreement",
		},
		Log: log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a, mock)

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

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	a := &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(srv.URL, "test-key", 2*time.Second),
		cfg:    &AdapterConfig{},
		Log:    log.NewTLog("VoiceAdapterTest"),
	}
	rec := callGetConfig(a, mock)

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

func TestGetConfigHandler_WithSpaceID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("subject_id") != "user1" {
			t.Errorf("expected subject_id=user1, got %s", r.URL.Query().Get("subject_id"))
		}
		if r.URL.Query().Get("scope_type") != "space" {
			t.Errorf("expected scope_type=space, got %s", r.URL.Query().Get("scope_type"))
		}
		if r.URL.Query().Get("scope_id") != "space123" {
			t.Errorf("expected scope_id=space123, got %s", r.URL.Query().Get("scope_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":       true,
			"local_enabled": false,
		})
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	gc.Request.Header.Set("X-Space-ID", "space123")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getConfig(wkCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["local_enabled"] != false {
		t.Errorf("expected local_enabled=false, got %v", body["local_enabled"])
	}
}

func TestGetConfigHandler_MissingSpaceIDHeader(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.getConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "X-Space-ID header is required" {
		t.Errorf("expected msg='X-Space-ID header is required', got %v", body["msg"])
	}
}

func TestGetConfigHandler_QuerySpaceIDIgnored(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config?space_id=space123", nil)
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.getConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (query param ignored), got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/v1/speech/local-config" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":200,"msg":"ok"}`))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":true}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_MissingEnabled(t *testing.T) {
	ctx, _, cleanup := newTestContextWithMockDB(t)
	defer cleanup()

	a := newTestAdapterWithDB("http://unused", ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_MissingSpaceIDHeader(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":true}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "X-Space-ID header is required" {
		t.Errorf("expected msg='X-Space-ID header is required', got %v", body["msg"])
	}
}

func TestPutLocalConfigHandler_QuerySpaceIDIgnored(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":true}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config?space_id=space1", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (query param ignored), got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_4xxPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte("validation failed"))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":true}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_5xxReturnsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":false}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestPutLocalConfigHandler_NetworkErrorReturnsBadGateway(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB("http://"+addr, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	reqBody := `{"enabled":true}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(reqBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestGetLocalConfigHandler_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":    true,
			"timeout_ms": 5000,
		})
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/local-config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getLocalConfig(wkCtx)

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
}

func TestGetLocalConfigHandler_MissingSpaceIDHeader(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/local-config", nil)
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.getLocalConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "X-Space-ID header is required" {
		t.Errorf("expected msg='X-Space-ID header is required', got %v", body["msg"])
	}
}

func TestGetLocalConfigHandler_QuerySpaceIDIgnored(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/local-config?scope_type=space&scope_id=space1", nil)
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.getLocalConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (query params ignored), got %d", rec.Code)
	}
}

func TestGetLocalConfigHandler_4xxPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/local-config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getLocalConfig(wkCtx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestDeleteLocalConfigHandler_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":200,"msg":"ok"}`))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodDelete, "/v1/voice/local-config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.deleteLocalConfig(wkCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestDeleteLocalConfigHandler_MissingSpaceIDHeader(t *testing.T) {
	a := newTestAdapter("http://unused")
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodDelete, "/v1/voice/local-config", nil)
	gc.Set("uid", "user1")
	ctx := &wkhttp.Context{Context: gc}
	a.deleteLocalConfig(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "X-Space-ID header is required" {
		t.Errorf("expected msg='X-Space-ID header is required', got %v", body["msg"])
	}
}

func TestDeleteLocalConfigHandler_5xxReturnsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	a := newTestAdapterWithDB(srv.URL, ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodDelete, "/v1/voice/local-config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.deleteLocalConfig(wkCtx)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestGetConfig_NonMember(t *testing.T) {
	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	a := newTestAdapterWithDB("http://unused", ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getConfig(wkCtx)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "not a member of this space" {
		t.Errorf("expected msg='not a member of this space', got %v", body["msg"])
	}
}

func TestGetConfig_MembershipCheckError(t *testing.T) {
	ctx, mock, cleanup := newTestContextWithMockDB(t)
	defer cleanup()
	mock.ExpectQuery("SELECT COUNT").WillReturnError(sqlmock.ErrCancelled)

	a := newTestAdapterWithDB("http://unused", ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/config", nil)
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getConfig(wkCtx)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "check space membership failed" {
		t.Errorf("expected msg='check space membership failed', got %v", body["msg"])
	}
}

func TestPutLocalConfig_BodyTooLarge(t *testing.T) {
	ctx, _, cleanup := newTestContextWithMockDB(t)
	defer cleanup()

	a := newTestAdapterWithDB("http://unused", ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	largeBody := `{"enabled":true,"probe_url":"` + strings.Repeat("a", 65*1024) + `"}`
	gc.Request = httptest.NewRequest(http.MethodPut, "/v1/voice/local-config", bytes.NewBufferString(largeBody))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Request.Header.Set("X-Space-ID", "space1")
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.putLocalConfig(wkCtx)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "request body too large" {
		t.Errorf("expected msg='request body too large', got %v", body["msg"])
	}
}

func TestGetContext_QuerySpaceIDIgnored(t *testing.T) {
	ctx, _, cleanup := newTestContextWithMockDB(t)
	defer cleanup()

	a := newTestAdapterWithDB("http://unused", ctx)
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest(http.MethodGet, "/v1/voice/context?space_id=space1", nil)
	gc.Set("uid", "user1")
	wkCtx := &wkhttp.Context{Context: gc}
	a.getContext(wkCtx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (query param ignored), got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["msg"] != "X-Space-ID header is required" {
		t.Errorf("expected msg='X-Space-ID header is required', got %v", body["msg"])
	}
}
