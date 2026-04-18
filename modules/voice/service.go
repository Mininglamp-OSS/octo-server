package voice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// VoiceService handles voice transcription via LiteLLM
type VoiceService struct {
	config *VoiceConfig
	client *http.Client
}

// NewVoiceService creates a new VoiceService
func NewVoiceService(cfg *VoiceConfig) *VoiceService {
	return &VoiceService{
		config: cfg,
		client: &http.Client{},
	}
}

// effectiveURL returns the API base URL for the current engine.
func (s *VoiceService) effectiveURL() string {
	if s.config.Engine == EngineQwen && s.config.QwenUrl != "" {
		return s.config.QwenUrl
	}
	return s.config.LiteLLMUrl
}

// effectiveKey returns the API key for the current engine.
func (s *VoiceService) effectiveKey() string {
	if s.config.Engine == EngineQwen && s.config.QwenKey != "" {
		return s.config.QwenKey
	}
	return s.config.LiteLLMKey
}

// effectiveTimeout returns the per-model timeout for the current engine.
func (s *VoiceService) effectiveTimeout() int {
	if s.config.Engine == EngineQwen && s.config.QwenTimeout > 0 {
		return s.config.QwenTimeout
	}
	return s.config.Timeout
}

// chatCompletionModels returns the model fallback chain for the current
// chat/completions engine (Gemini or Qwen). Not used by the GPT engine,
// which has its own model list (GPTModels) and uses audio/transcriptions.
func (s *VoiceService) chatCompletionModels() []string {
	switch s.config.Engine {
	case EngineQwen:
		return s.config.QwenModels
	default: // gemini
		return s.config.Models
	}
}

// TranscribeOptions holds per-request overrides for transcription
type TranscribeOptions struct {
	// Mode overrides the transcription mode: "append" or "edit".
	// Empty string uses the global VoiceConfig.EditMode.
	Mode string

	// Model overrides the preferred model.
	// Empty string uses the global fallback chain.
	Model string
}

// TranscribeResult holds the transcription result along with metadata for logging
type TranscribeResult struct {
	Text         string      // post-processed final result
	RawText      string      // raw model output (before post-processing)
	Model        string      // actual model used
	PromptText   string      // user message text sent to model
	SystemPrompt string      // system message sent to model (empty for GPT engine)
	PromptType   string      // "chat_completion" or "audio_transcription"
	RequestBody  interface{} // full request body sent to LiteLLM (JSON-serializable)
}

// Transcribe dispatches to append or edit path based on EditMode.
func (s *VoiceService) Transcribe(audioData []byte, mimeType string, contextText string, chatContext string) (string, string, error) {
	return s.TranscribeWithOptions(audioData, mimeType, contextText, chatContext, TranscribeOptions{})
}

// ErrGPTEditNotSupported is returned when edit mode is requested with GPT engine.
var ErrGPTEditNotSupported = fmt.Errorf("edit mode is not supported with GPT engine")

// TranscribeWithOptions supports per-request mode/model override.
// Internally delegates to TranscribeWithResult.
func (s *VoiceService) TranscribeWithOptions(audioData []byte, mimeType, contextText, chatContext string, opts TranscribeOptions) (string, string, error) {
	result, err := s.TranscribeWithResult(audioData, mimeType, contextText, chatContext, opts)
	if err != nil {
		return "", "", err
	}
	return result.Text, result.Model, nil
}

// TranscribeWithResult is like TranscribeWithOptions but returns additional metadata
// (prompt text, request body, raw text) needed for ASR data collection.
func (s *VoiceService) TranscribeWithResult(audioData []byte, mimeType, contextText, chatContext string,
	opts TranscribeOptions) (*TranscribeResult, error) {

	mode := s.config.EditMode
	if opts.Mode != "" {
		mode = opts.Mode
	}

	if s.config.Engine == EngineGPT && mode == "edit" {
		return nil, ErrGPTEditNotSupported
	}

	svc := s
	if opts.Model != "" {
		cfgCopy := *s.config
		switch s.config.Engine {
		case EngineGPT:
			cfgCopy.GPTModels = append([]string{opts.Model}, s.config.GPTModels...)
		case EngineQwen:
			cfgCopy.QwenModels = append([]string{opts.Model}, s.config.QwenModels...)
		default: // gemini
			cfgCopy.Models = append([]string{opts.Model}, s.config.Models...)
		}
		svc = &VoiceService{config: &cfgCopy, client: s.client}
	}

	// Build prompts
	userMsg := buildUserMessage(mode, contextText, chatContext)
	var systemMsg string
	if svc.config.Engine != EngineGPT {
		systemMsg = buildSystemMessage()
	}

	// Call model
	var rawText, model string
	var requestBody interface{}
	var promptType string
	var err error

	switch svc.config.Engine {
	case EngineGPT:
		promptType = "audio_transcription"
		rawText, model, requestBody, err = svc.callGPTWithModelFallback(audioData, mimeType, userMsg)
	default:
		promptType = "chat_completion"
		rawText, model, requestBody, err = svc.callChatCompletionWithFallback(audioData, mimeType,
			systemMsg, userMsg, svc.chatCompletionModels())
	}
	if err != nil {
		return &TranscribeResult{
			PromptText:   userMsg,
			SystemPrompt: systemMsg,
			PromptType:   promptType,
			RequestBody:  requestBody,
		}, err
	}

	result := &TranscribeResult{
		RawText:      rawText,
		Text:         rawText,
		Model:        model,
		PromptText:   userMsg,
		SystemPrompt: systemMsg,
		PromptType:   promptType,
		RequestBody:  requestBody,
	}

	// Post-processing: append and edit have different NoSpeech semantics
	switch mode {
	case "append":
		if IsNoSpeech(rawText) {
			if contextText != "" {
				result.Text = contextText
			} else {
				result.Text = ""
			}
		} else if contextText != "" {
			result.Text = joinContextAndText(contextText, rawText)
		}
	default: // edit
		if rawText == noSpeechSentinel {
			if contextText != "" {
				result.Text = contextText
			} else {
				result.Text = ""
			}
		} else if contextText != "" {
			result.Text = restoreTrailingWhitespace(contextText, rawText)
		}
	}

	return result, nil
}

// callChatCompletionWithFallback wraps callChatCompletion with model loop + total timeout.
func (s *VoiceService) callChatCompletionWithFallback(audioData []byte, mimeType string,
	systemMsg, userMsg string, models []string) (string, string, interface{}, error) {

	totalCtx, totalCancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.TotalTimeout)*time.Second)
	defer totalCancel()

	var lastErr error
	var lastReqBody interface{}
	for _, model := range models {
		if totalCtx.Err() != nil {
			break
		}

		text, reqBody, err := s.callChatCompletion(totalCtx, model, audioData, mimeType, systemMsg, userMsg)
		lastReqBody = reqBody
		if err == nil {
			return text, model, reqBody, nil
		}

		lastErr = err
		if isNonRetryableError(err) {
			return "", model, reqBody, err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no models configured")
	}
	return "", "", lastReqBody, fmt.Errorf("all models failed: %w", lastErr)
}

// callGPTWithModelFallback wraps callAudioTranscriptions with model loop + total timeout.
// Returns requestBody containing audio_base64 for self-contained JSON logging.
func (s *VoiceService) callGPTWithModelFallback(audioData []byte, mimeType string,
	prompt string) (string, string, interface{}, error) {

	totalCtx, totalCancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.TotalTimeout)*time.Second)
	defer totalCancel()

	audioBase64 := base64.StdEncoding.EncodeToString(audioData)

	var lastErr error
	var lastReqBody interface{}
	for _, model := range s.config.GPTModels {
		if totalCtx.Err() != nil {
			break
		}

		text, err := s.callAudioTranscriptions(totalCtx, model, audioData, mimeType, prompt)

		reqBody := map[string]interface{}{
			"model":        model,
			"language":     s.config.Language,
			"prompt":       prompt,
			"file":         "(multipart binary, see audio_file in input)",
			"audio_base64": audioBase64,
		}
		lastReqBody = reqBody

		if err == nil {
			return text, model, reqBody, nil
		}

		lastErr = err
		if isNonRetryableError(err) {
			return "", model, reqBody, err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no GPT models configured")
	}
	return "", "", lastReqBody, fmt.Errorf("all GPT models failed: %w", lastErr)
}

// callChatCompletion sends a chat completion request with audio content.
// Used by both Gemini and Qwen engines. Qwen Omni natively supports the
// OpenAI-compatible input_audio content part, so the same request format
// works for both engines without any adaptation.
func (s *VoiceService) callChatCompletion(totalCtx context.Context, model string, audioData []byte, mimeType string, systemMsg, userMsg string) (string, interface{}, error) {
	b64Audio := base64.StdEncoding.EncodeToString(audioData)

	// DashScope (Qwen) requires data URI format: "data:;base64,{base64}"
	if s.config.Engine == EngineQwen {
		b64Audio = "data:;base64," + b64Audio
	}

	// Only use reasoning_effort=low for Gemini 3.1 Pro (reduces latency without hurting quality)
	var reasoningEffort string
	if s.config.Engine == EngineGemini && strings.Contains(model, "3.1-pro") {
		reasoningEffort = "low"
	}

	var messages []message
	if systemMsg != "" {
		messages = append(messages, message{Role: "system", Content: systemMsg})
	}
	messages = append(messages, message{
		Role: "user",
		Content: []contentPart{
			{
				Type: "text",
				Text: userMsg,
			},
			{
				Type: "input_audio",
				InputAudio: &inputAudio{
					Data:   b64Audio,
					Format: mimeTypeToFormat(mimeType),
				},
			},
		},
	})

	reqBody := chatCompletionRequest{
		Model:           model,
		ReasoningEffort: reasoningEffort,
		Messages:        messages,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", reqBody, fmt.Errorf("marshal request: %w", err)
	}

	perModelTimeout := time.Duration(s.effectiveTimeout()) * time.Second
	ctx, cancel := context.WithTimeout(totalCtx, perModelTimeout)
	defer cancel()

	url := strings.TrimRight(s.effectiveURL(), "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", reqBody, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.effectiveKey())

	resp, err := s.client.Do(req)
	if err != nil {
		return "", reqBody, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", reqBody, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", reqBody, &apiError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var chatResp chatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", reqBody, fmt.Errorf("parse response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", reqBody, fmt.Errorf("empty response from model")
	}

	result := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	return result, reqBody, nil
}

// callAudioTranscriptions sends audio to the OpenAI-compatible audio transcriptions API.
func (s *VoiceService) callAudioTranscriptions(totalCtx context.Context, model string,
	audioData []byte, mimeType string, prompt string) (string, error) {

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	writer.WriteField("model", model)

	if s.config.Language != "" {
		writer.WriteField("language", s.config.Language)
	}

	if prompt != "" {
		writer.WriteField("prompt", prompt)
	}

	ext := mimeTypeToFormat(mimeType)
	part, err := writer.CreateFormFile("file", "audio."+ext)
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	part.Write(audioData)
	writer.Close()

	perModelTimeout := time.Duration(s.config.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(totalCtx, perModelTimeout)
	defer cancel()

	url := strings.TrimRight(s.config.LiteLLMUrl, "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+s.config.LiteLLMKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", &apiError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return strings.TrimSpace(result.Text), nil
}

// joinContextAndText joins existing text and transcription result.
// CJK characters (incl. Japanese kana, Korean) don't get a space between them.
func joinContextAndText(contextText, newText string) string {
	if contextText == "" || newText == "" {
		return contextText + newText
	}
	ctxRunes := []rune(contextText)
	newRunes := []rune(newText)
	last := ctxRunes[len(ctxRunes)-1]
	first := newRunes[0]

	// Trailing space or punctuation → join directly
	if unicode.IsSpace(last) || isPunctuation(last) {
		return contextText + newText
	}
	// Either side is CJK → no space
	if isCJK(last) || isCJK(first) {
		return contextText + newText
	}
	// Other (English etc.) → add space
	return contextText + " " + newText
}

// isCJK detects CJK unified ideographs, Japanese kana, Korean syllables, and related symbols.
func isCJK(r rune) bool {
	return (r >= 0x4e00 && r <= 0x9fff) || // CJK Unified Ideographs
		(r >= 0x3400 && r <= 0x4dbf) || // CJK Extension A
		(r >= 0x3000 && r <= 0x303f) || // CJK Symbols and Punctuation
		(r >= 0xff00 && r <= 0xffef) || // Fullwidth Forms
		(r >= 0x3040 && r <= 0x309f) || // Hiragana
		(r >= 0x30a0 && r <= 0x30ff) || // Katakana
		(r >= 0xac00 && r <= 0xd7af) // Hangul Syllables
}

func isPunctuation(r rune) bool {
	return strings.ContainsRune(`，。！？；：、,.!?;:…—·"'）」】》〉`+"\u201C\u201D\u2018\u2019", r)
}

// restoreTrailingWhitespace restores trailing whitespace stripped by LLM.
// Append scenario (HasPrefix match): whitespace restored between original and appended text.
// Edit scenario (no match): whitespace appended to the end.
func restoreTrailingWhitespace(contextText, text string) string {
	trimmedCtx := strings.TrimRight(contextText, " \t\n\r")
	trailing := contextText[len(trimmedCtx):]

	if trailing == "" || trimmedCtx == "" {
		return text
	}

	if strings.HasPrefix(text, trimmedCtx) {
		// Append scenario: original preserved, restore whitespace in between
		rest := text[len(trimmedCtx):]
		return trimmedCtx + trailing + strings.TrimLeft(rest, " \t")
	}

	// Edit scenario: original was modified, restore trailing whitespace
	return strings.TrimRight(text, " \t\n\r") + trailing
}

// mimeTypeToFormat converts MIME type to a short format string for the API
func mimeTypeToFormat(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "wav"):
		return "wav"
	case strings.Contains(mimeType, "mp3"), strings.Contains(mimeType, "mpeg"):
		return "mp3"
	case strings.Contains(mimeType, "ogg"):
		return "ogg"
	case strings.Contains(mimeType, "webm"):
		return "webm"
	case strings.Contains(mimeType, "mp4"), strings.Contains(mimeType, "m4a"):
		return "m4a"
	case strings.Contains(mimeType, "flac"):
		return "flac"
	default:
		return "wav"
	}
}

// apiError represents an HTTP error from the LiteLLM API
type apiError struct {
	StatusCode int
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

// isNonRetryableError returns true for 4xx errors other than 429
func isNonRetryableError(err error) bool {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode >= 400 && ae.StatusCode < 500 && ae.StatusCode != 429
	}
	return false
}

// Request/response types for OpenAI-compatible chat completion API

type chatCompletionRequest struct {
	Model           string    `json:"model"`
	Messages        []message `json:"messages"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

// message represents a chat completion message. Content is string for system
// messages and []contentPart for user messages with audio.
type message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// UnmarshalJSON handles decoding both string content (system) and array
// content (user) from JSON.
func (m *message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role

	// Try string first (system message)
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}

	// Try []contentPart (user message)
	var parts []contentPart
	if err := json.Unmarshal(raw.Content, &parts); err == nil {
		m.Content = parts
		return nil
	}

	return fmt.Errorf("message content is neither string nor []contentPart")
}

type contentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	InputAudio *inputAudio `json:"input_audio,omitempty"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type chatCompletionResponse struct {
	Choices []choice `json:"choices"`
}

type choice struct {
	Message responseMessage `json:"message"`
}

type responseMessage struct {
	Content string `json:"content"`
}
