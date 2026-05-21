package voice_adapter

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewSpeechClient(t *testing.T) {
	c := NewSpeechClient("http://localhost:8780/", "test-key", 30*time.Second)
	if c.baseURL != "http://localhost:8780" {
		t.Errorf("expected trailing slash trimmed, got %q", c.baseURL)
	}
	if c.apiKey != "test-key" {
		t.Errorf("expected apiKey 'test-key', got %q", c.apiKey)
	}
}

func TestForwardTranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/speech/transcribe" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": 200,
			"text":   "hello world",
		})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/voice/transcribe", strings.NewReader("audio-data"))
	req.Header.Set("Content-Type", "multipart/form-data")

	resp, err := client.ForwardTranscribe(req)
	if err != nil {
		t.Fatalf("ForwardTranscribe failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestGetVocabulary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/speech/vocabularies" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("subject_id") != "user1" {
			t.Errorf("unexpected subject_id: %s", r.URL.Query().Get("subject_id"))
		}
		if r.URL.Query().Get("scope_type") != "space" {
			t.Errorf("unexpected scope_type: %s", r.URL.Query().Get("scope_type"))
		}
		if r.URL.Query().Get("scope_id") != "space1" {
			t.Errorf("unexpected scope_id: %s", r.URL.Query().Get("scope_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(VocabularyResponse{
			HasContent: true,
			Content:    "test context",
			UpdatedAt:  "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	vocab, err := client.GetVocabulary(context.Background(), "user1", "space", "space1")
	if err != nil {
		t.Fatalf("GetVocabulary failed: %v", err)
	}
	if !vocab.HasContent {
		t.Error("expected has_content=true")
	}
	if vocab.Content != "test context" {
		t.Errorf("expected 'test context', got %q", vocab.Content)
	}
	if vocab.UpdatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("unexpected updated_at: %s", vocab.UpdatedAt)
	}
}

func TestGetVocabularyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	_, err := client.GetVocabulary(context.Background(), "user1", "space", "space1")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status code 500, got: %s", err.Error())
	}
}

func TestPutVocabulary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if r.URL.Path != "/v1/speech/vocabularies" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json content-type, got %s", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		var req PutVocabularyRequest
		json.Unmarshal(body, &req)
		if req.SubjectID != "user1" || req.ScopeType != "space" || req.ScopeID != "space1" {
			t.Errorf("unexpected request body: %s", string(body))
		}
		if req.Content != "test content" || req.UpdatedBy != "bot1" {
			t.Errorf("unexpected content or updated_by: %s", string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": 200,
			"msg":    "ok",
		})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	err := client.PutVocabulary(context.Background(), PutVocabularyRequest{
		SubjectID: "user1",
		ScopeType: "space",
		ScopeID:   "space1",
		Content:   "test content",
		UpdatedBy: "bot1",
	})
	if err != nil {
		t.Fatalf("PutVocabulary failed: %v", err)
	}
}

func TestDeleteVocabulary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/v1/speech/vocabularies" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": 200,
			"msg":    "ok",
		})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	err := client.DeleteVocabulary(context.Background(), "user1", "space", "space1")
	if err != nil {
		t.Fatalf("DeleteVocabulary failed: %v", err)
	}
}

func TestGetConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/speech/config" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":      true,
			"max_duration": 60,
			"feedback_url": "https://example.com/feedback",
		})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	cfg, err := client.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig failed: %v", err)
	}
	if cfg["enabled"] != true {
		t.Error("expected enabled=true")
	}
	if cfg["feedback_url"] != "https://example.com/feedback" {
		t.Errorf("unexpected feedback_url: %v", cfg["feedback_url"])
	}
}

func TestGetConfigError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service down"))
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	_, err := client.GetConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
}

func TestGetConfig_ConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	client := NewSpeechClient("http://"+addr, "test-key", 2*time.Second)
	_, err = client.GetConfig(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestGetVocabulary_URLEscaping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("subject_id") != "user&id=1" {
			t.Errorf("subject_id not properly decoded: %q", r.URL.Query().Get("subject_id"))
		}
		if r.URL.Query().Get("scope_id") != "space with spaces" {
			t.Errorf("scope_id not properly decoded: %q", r.URL.Query().Get("scope_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(VocabularyResponse{HasContent: false})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	_, err := client.GetVocabulary(context.Background(), "user&id=1", "space", "space with spaces")
	if err != nil {
		t.Fatalf("GetVocabulary with special chars failed: %v", err)
	}
}

func TestDeleteVocabulary_URLEscaping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("subject_id") != "user&id=1" {
			t.Errorf("subject_id not properly decoded: %q", r.URL.Query().Get("subject_id"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":200}`))
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	err := client.DeleteVocabulary(context.Background(), "user&id=1", "space", "space1")
	if err != nil {
		t.Fatalf("DeleteVocabulary with special chars failed: %v", err)
	}
}

func TestForwardTranscribeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/speech/transcribe" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("expected multipart content-type, got %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			t.Error("expected non-empty body")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status": 200, "text": "ok"})
	}))
	defer srv.Close()

	client := NewSpeechClient(srv.URL, "test-key", 5*time.Second)
	resp, err := client.ForwardTranscribeBody(
		context.Background(),
		strings.NewReader("test-audio-data"),
		"multipart/form-data; boundary=test",
	)
	if err != nil {
		t.Fatalf("ForwardTranscribeBody failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}
