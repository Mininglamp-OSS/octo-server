package voice_adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SpeechClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewSpeechClient(baseURL, apiKey string, timeout time.Duration) *SpeechClient {
	return &SpeechClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  &http.Client{Timeout: timeout},
	}
}

type VocabularyResponse struct {
	HasContent bool   `json:"has_content"`
	Content    string `json:"content"`
	UpdatedAt  string `json:"updated_at"`
}

type PutVocabularyRequest struct {
	SubjectID string `json:"subject_id"`
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
	Content   string `json:"content"`
	UpdatedBy string `json:"updated_by"`
}

func (c *SpeechClient) ForwardTranscribe(r *http.Request) (*http.Response, error) {
	return c.ForwardTranscribeBody(r.Context(), r.Body, r.Header.Get("Content-Type"))
}

func (c *SpeechClient) ForwardTranscribeBody(ctx context.Context, body io.Reader, contentType string) (*http.Response, error) {
	target := c.baseURL + "/v1/speech/transcribe"

	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, fmt.Errorf("create proxy request: %w", err)
	}
	proxyReq.Header.Set("Content-Type", contentType)
	proxyReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	return c.client.Do(proxyReq)
}

func (c *SpeechClient) GetVocabulary(subjectID, scopeType, scopeID string) (*VocabularyResponse, error) {
	params := url.Values{}
	params.Set("subject_id", subjectID)
	params.Set("scope_type", scopeType)
	params.Set("scope_id", scopeID)
	reqURL := c.baseURL + "/v1/speech/vocabularies?" + params.Encode()

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("speech service returned %d: %s", resp.StatusCode, string(body))
	}

	var result VocabularyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

func (c *SpeechClient) PutVocabulary(req PutVocabularyRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPut, c.baseURL+"/v1/speech/vocabularies", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("speech service returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *SpeechClient) DeleteVocabulary(subjectID, scopeType, scopeID string) error {
	params := url.Values{}
	params.Set("subject_id", subjectID)
	params.Set("scope_type", scopeType)
	params.Set("scope_id", scopeID)
	reqURL := c.baseURL + "/v1/speech/vocabularies?" + params.Encode()

	req, err := http.NewRequest(http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("speech service returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *SpeechClient) GetConfig() (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/v1/speech/config", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("speech service returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}
