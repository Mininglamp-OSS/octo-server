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

// TranscribeOptions holds per-request overrides for transcription
type TranscribeOptions struct {
	// Mode overrides the transcription mode: "append" or "edit".
	// Empty string uses the global VoiceConfig.EditMode.
	Mode string

	// Model overrides the preferred model.
	// Empty string uses the global fallback chain.
	Model string
}

// Transcribe dispatches to append or edit path based on EditMode.
func (s *VoiceService) Transcribe(audioData []byte, mimeType string, contextText string, chatContext string) (string, string, error) {
	return s.TranscribeWithOptions(audioData, mimeType, contextText, chatContext, TranscribeOptions{})
}

// ErrGPTEditNotSupported is returned when edit mode is requested with GPT engine.
var ErrGPTEditNotSupported = fmt.Errorf("edit mode is not supported with GPT engine")

// TranscribeWithOptions supports per-request mode/model override.
// Empty option fields fall back to the global configuration.
func (s *VoiceService) TranscribeWithOptions(audioData []byte, mimeType, contextText, chatContext string, opts TranscribeOptions) (string, string, error) {
	mode := s.config.EditMode
	if opts.Mode != "" {
		mode = opts.Mode
	}

	if s.config.Engine == "gpt" && mode == "edit" {
		return "", "", ErrGPTEditNotSupported
	}

	svc := s
	if opts.Model != "" {
		cfgCopy := *s.config
		if s.config.Engine == "gpt" {
			cfgCopy.GPTModels = append([]string{opts.Model}, s.config.GPTModels...)
		} else {
			cfgCopy.Models = append([]string{opts.Model}, s.config.Models...)
		}
		svc = &VoiceService{config: &cfgCopy, client: s.client}
	}

	switch mode {
	case "append":
		return svc.transcribeAppend(audioData, mimeType, contextText, chatContext)
	default: // "edit"
		return svc.transcribeEdit(audioData, mimeType, contextText, chatContext)
	}
}

// transcribeAppend — pure transcription + backend join, used by both engines.
// NO_SPEECH: both empty and [NO_SPEECH] sentinel are treated as silence.
func (s *VoiceService) transcribeAppend(audioData []byte, mimeType string,
	contextText string, chatContext string) (string, string, error) {

	prompt := buildAppendPrompt(contextText, chatContext)

	var text, model string
	var err error
	switch s.config.Engine {
	case "gpt":
		text, model, err = s.callGPTWithModelFallback(audioData, mimeType, prompt)
	default: // gemini
		text, model, err = s.callWithModelFallback(audioData, mimeType, prompt)
	}
	if err != nil {
		return "", "", err
	}

	// NO_SPEECH: both empty and sentinel treated as silence
	if isNoSpeech(text) {
		if contextText != "" {
			return contextText, model, nil
		}
		return "", model, nil
	}

	// Backend join
	if contextText != "" {
		text = joinContextAndText(contextText, text)
	}

	return text, model, nil
}

// transcribeEdit — Gemini only, LLM performs editing + whitespace restore.
// NO_SPEECH: only [NO_SPEECH] sentinel is silence; empty string is legitimate "delete everything".
func (s *VoiceService) transcribeEdit(audioData []byte, mimeType string,
	contextText string, chatContext string) (string, string, error) {

	prompt := buildPrompt(contextText, chatContext)

	text, model, err := s.callWithModelFallback(audioData, mimeType, prompt)
	if err != nil {
		return "", "", err
	}

	// Only sentinel counts as silence; empty string = "delete everything"
	if text == noSpeechSentinel {
		if contextText != "" {
			return contextText, model, nil
		}
		return "", model, nil
	}

	// Restore trailing whitespace that LLM may have stripped
	if contextText != "" {
		text = restoreTrailingWhitespace(contextText, text)
	}

	return text, model, nil
}

// callWithModelFallback wraps callLiteLLM with model loop + total timeout.
func (s *VoiceService) callWithModelFallback(audioData []byte, mimeType string,
	prompt string) (string, string, error) {

	totalCtx, totalCancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.TotalTimeout)*time.Second)
	defer totalCancel()

	var lastErr error
	for _, model := range s.config.Models {
		if totalCtx.Err() != nil {
			break
		}

		text, err := s.callLiteLLM(totalCtx, model, audioData, mimeType, prompt)
		if err == nil {
			return text, model, nil
		}

		lastErr = err
		if isNonRetryableError(err) {
			return "", model, err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no models configured")
	}
	return "", "", fmt.Errorf("all models failed: %w", lastErr)
}

// callGPTWithModelFallback wraps callAudioTranscriptions with model loop + total timeout.
func (s *VoiceService) callGPTWithModelFallback(audioData []byte, mimeType string,
	prompt string) (string, string, error) {

	totalCtx, totalCancel := context.WithTimeout(context.Background(),
		time.Duration(s.config.TotalTimeout)*time.Second)
	defer totalCancel()

	var lastErr error
	for _, model := range s.config.GPTModels {
		if totalCtx.Err() != nil {
			break
		}

		text, err := s.callAudioTranscriptions(totalCtx, model, audioData, mimeType, prompt)
		if err == nil {
			return text, model, nil
		}

		lastErr = err
		if isNonRetryableError(err) {
			return "", model, err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no GPT models configured")
	}
	return "", "", fmt.Errorf("all GPT models failed: %w", lastErr)
}

// callLiteLLM sends a chat completion request to LiteLLM with audio content.
func (s *VoiceService) callLiteLLM(totalCtx context.Context, model string, audioData []byte, mimeType string, prompt string) (string, error) {
	b64Audio := base64.StdEncoding.EncodeToString(audioData)

	// Only use reasoning_effort=low for Gemini 3.1 Pro (reduces latency without hurting quality)
	var reasoningEffort string
	if strings.Contains(model, "3.1-pro") {
		reasoningEffort = "low"
	}

	reqBody := chatCompletionRequest{
		Model:           model,
		ReasoningEffort: reasoningEffort,
		Messages: []message{
			{
				Role: "user",
				Content: []contentPart{
					{
						Type: "text",
						Text: prompt,
					},
					{
						Type: "input_audio",
						InputAudio: &inputAudio{
							Data:   b64Audio,
							Format: mimeTypeToFormat(mimeType),
						},
					},
				},
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	perModelTimeout := time.Duration(s.config.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(totalCtx, perModelTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(s.config.LiteLLMUrl, "/")+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
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
		return "", &apiError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	var chatResp chatCompletionResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("empty response from model")
	}

	result := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	return result, nil
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
	Model            string            `json:"model"`
	Messages         []message         `json:"messages"`
	ReasoningEffort  string            `json:"reasoning_effort,omitempty"`
}

type message struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
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
