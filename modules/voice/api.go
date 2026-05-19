package voice

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// globalASRLogger is the singleton ASRLogger shared between voice and botfather modules.
var globalASRLogger *ASRLogger

// GetASRLogger returns the global ASRLogger singleton (nil if ASR logging is disabled).
func GetASRLogger() *ASRLogger {
	return globalASRLogger
}

// Voice is the API handler for voice transcription
type Voice struct {
	ctx       *config.Context
	service   *VoiceService
	cfg       *VoiceConfig
	db        VoiceStore
	asrLogger *ASRLogger
	log.Log
}

// New creates a new Voice API handler
func New(ctx *config.Context, cfg *VoiceConfig) *Voice {
	return &Voice{
		ctx:     ctx,
		service: NewVoiceService(cfg),
		cfg:     cfg,
		db:      NewVoiceDB(ctx),
		Log:     log.NewTLog("Voice"),
	}
}

// Route registers voice API routes
func (v *Voice) Route(r *wkhttp.WKHttp) {
	// Load configurable prompts (only when VOICE_PROMPT_FILE is set)
	if v.cfg.PromptFile != "" {
		LoadPrompts(v.cfg.PromptFile, v.Log)
	}

	auth := r.Group("/v1/voice", v.ctx.AuthMiddleware(r))
	{
		auth.POST("/transcribe", v.transcribe)
		auth.GET("/context", v.getContext)
	}

	open := r.Group("/v1/voice")
	{
		open.GET("/config", v.getConfig)
	}
}

// transcribe handles voice transcription requests
func (v *Voice) transcribe(c *wkhttp.Context) {
	file, header, err := c.Request.FormFile("audio")
	if err != nil {
		c.ResponseError(errors.New("audio file is required"))
		return
	}
	defer file.Close()

	if header.Size > v.cfg.MaxFileSize {
		c.ResponseErrorWithStatus(errors.New("file size exceeds limit"), http.StatusBadRequest)
		return
	}

	audioData, err := io.ReadAll(file)
	if err != nil {
		v.Error("failed to read audio file", zap.Error(err))
		c.ResponseError(errors.New("failed to read audio file"))
		return
	}

	// Detect MIME type from file header (first 512 bytes)
	mimeType := http.DetectContentType(audioData)
	// If DetectContentType returns generic octet-stream, try the upload header
	if mimeType == "application/octet-stream" && header.Header.Get("Content-Type") != "" {
		mimeType = header.Header.Get("Content-Type")
	}

	contextText := c.Request.FormValue("context_text")
	if len([]rune(contextText)) > v.cfg.MaxContextTextLength {
		v.Warn("context_text exceeds max length, truncating to keep recent text",
			zap.Int("original_rune_length", len([]rune(contextText))),
			zap.Int("max_length", v.cfg.MaxContextTextLength))
		contextText = TruncateRunesTail(contextText, v.cfg.MaxContextTextLength)
	}

	chatContext := c.Request.FormValue("chat_context")
	if len([]rune(chatContext)) > v.cfg.MaxChatContextLength {
		v.Warn("chat_context exceeds max length, truncating to last characters",
			zap.Int("original_rune_length", len([]rune(chatContext))),
			zap.Int("max_length", v.cfg.MaxChatContextLength))
		chatContext = TruncateRunesTail(chatContext, v.cfg.MaxChatContextLength)
	}

	personalContext := c.Request.FormValue("personal_context")
	if len([]rune(personalContext)) > v.cfg.MaxVoiceContextLength {
		v.Warn("personal_context exceeds max length, truncating to keep recent text",
			zap.Int("original_rune_length", len([]rune(personalContext))),
			zap.Int("max_length", v.cfg.MaxVoiceContextLength))
		personalContext = TruncateRunesTail(personalContext, v.cfg.MaxVoiceContextLength)
	}

	memberContext := c.Request.FormValue("member_context")
	if len([]rune(memberContext)) > v.cfg.MaxMemberContextLength {
		v.Warn("member_context exceeds max length, truncating to keep recent text",
			zap.Int("original_rune_length", len([]rune(memberContext))),
			zap.Int("max_length", v.cfg.MaxMemberContextLength))
		memberContext = TruncateRunesTail(memberContext, v.cfg.MaxMemberContextLength)
	}

	channelType := c.Request.FormValue("channel_type")
	if channelType == "" {
		channelType = "2"
	}
	skipMention := channelType == "1"
	if skipMention {
		memberContext = ""
	}

	// Parse and validate mode parameter
	mode := c.Request.FormValue("mode")
	var internalMode string
	switch mode {
	case "", "smart":
		// fallback to config.EditMode (internalMode stays empty)
	case "append_only":
		internalMode = "append"
	case "edit_only":
		if contextText == "" {
			c.ResponseErrorWithStatus(errors.New("edit_only mode requires context_text"), http.StatusBadRequest)
			return
		}
		// GPT engine does not support edit_only mode
		if v.cfg.Engine == EngineGPT {
			c.ResponseErrorWithStatus(ErrGPTEditNotSupported, http.StatusBadRequest)
			return
		}
		internalMode = "edit_only"
	default:
		c.ResponseErrorWithStatus(errors.New("invalid mode: must be smart, append_only, or edit_only"), http.StatusBadRequest)
		return
	}

	// Calculate effective mode for ASR logging
	effectiveMode := mode
	if effectiveMode == "" || effectiveMode == "smart" {
		effectiveMode = "smart"
	}

	// Save original chatContext for ASR logging
	origChatContext := chatContext

	// Merge vocabulary reference
	chatContext = BuildVocabularyReference(personalContext, memberContext, chatContext)

	startTime := time.Now()
	result, err := v.service.TranscribeWithResult(audioData, mimeType, contextText, chatContext,
		TranscribeOptions{Mode: internalMode, SkipMention: skipMention})
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		v.Error("transcription failed", zap.Error(err))
		var requestID string
		if v.asrLogger != nil {
			entry := v.buildASREntry("app", effectiveMode, audioData, mimeType, contextText, origChatContext,
				personalContext, memberContext, channelType, startTime, durationMs, result, err)
			requestID = entry.RequestID
			v.asrLogger.Enqueue(entry)
		} else {
			requestID = generateFallbackRequestID()
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status":     http.StatusInternalServerError,
			"msg":        "transcription failed",
			"request_id": requestID,
		})
		return
	}

	var requestID string
	if v.asrLogger != nil {
		entry := v.buildASREntry("app", effectiveMode, audioData, mimeType, contextText, origChatContext,
			personalContext, memberContext, channelType, startTime, durationMs, result, nil)
		requestID = entry.RequestID
		v.asrLogger.Enqueue(entry)
	} else {
		requestID = generateFallbackRequestID()
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     http.StatusOK,
		"text":       result.Text,
		"m":          shortenModelName(result.Model),
		"engine":     ShortenEngineName(v.cfg.Engine),
		"request_id": requestID,
	})
}

// getConfig returns voice feature configuration
func (v *Voice) getConfig(c *wkhttp.Context) {
	enabled := v.cfg.Validate() == nil
	c.JSON(http.StatusOK, gin.H{
		"enabled":              enabled,
		"max_duration":         v.cfg.MaxDuration,
		"max_file_size":        v.cfg.MaxFileSize,
		"engine":               ShortenEngineName(v.cfg.Engine),
		"edit_mode":            v.cfg.EditMode,
		"local_enabled":        v.cfg.LocalEnabled,
		"local_timeout_ms":     v.cfg.LocalTimeoutMs,
		"local_probe_url":      v.cfg.LocalProbeURL,
		"local_transcribe_url": v.cfg.LocalTranscribeURL,
	})
}

// buildASREntry constructs an ASREntry with common fields populated.
func (v *Voice) buildASREntry(source string, mode string, audioData []byte, mimeType string,
	contextText string, chatContext string, personalContext string, memberContext string, channelType string,
	startTime time.Time, durationMs int64,
	result *TranscribeResult, err error) ASREntry {

	entry := ASREntry{
		RequestID: v.asrLogger.GenerateRequestID(),
		Timestamp: startTime.UTC().Format(time.RFC3339Nano),
		Source:    source,
		Engine:    v.cfg.Engine,
		Input: ASRInput{
			Mode:            mode,
			MimeType:        mimeType,
			AudioSize:       len(audioData),
			ContextText:     contextText,
			ChatContext:     chatContext,
			PersonalContext: personalContext,
			MemberContext:   memberContext,
			ChannelType:     channelType,
		},
		AudioData:  audioData,
		DurationMs: durationMs,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	if result != nil {
		entry.ModelUsed = result.Model
		entry.Prompt = &ASRPrompt{
			Type:        result.PromptType,
			Text:        result.PromptText,
			RequestBody: result.RequestBody,
		}
		entry.RawResultText = result.RawText
		entry.ResultText = result.Text
		entry.ResultLength = len([]rune(result.Text))
		entry.IsNoSpeech = IsNoSpeech(result.RawText)
	}
	return entry
}

// getContext returns the user's personal voice correction context for the given space
func (v *Voice) getContext(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id is required"), http.StatusBadRequest)
		return
	}

	isMember, err := v.db.CheckSpaceMembership(spaceID, loginUID)
	if err != nil {
		v.Error("check space membership failed", zap.Error(err), zap.String("uid", loginUID), zap.String("spaceID", spaceID))
		c.ResponseErrorWithStatus(errors.New("check space membership failed"), http.StatusInternalServerError)
		return
	}
	if !isMember {
		c.ResponseErrorWithStatus(errors.New("no permission to access this space"), http.StatusForbidden)
		return
	}

	m, err := v.db.QueryVoiceContext(loginUID, spaceID)
	if err != nil {
		v.Error("query voice context failed", zap.Error(err), zap.String("uid", loginUID), zap.String("spaceID", spaceID))
		c.ResponseErrorWithStatus(errors.New("query voice context failed"), http.StatusInternalServerError)
		return
	}

	if m == nil {
		c.JSON(http.StatusOK, gin.H{
			"status":      http.StatusOK,
			"has_context": false,
			"context":     "",
			"updated_at":  "",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      http.StatusOK,
		"has_context": true,
		"context":     m.ASRCorrectContext,
		"updated_at":  m.UpdatedAt.Format(time.RFC3339),
	})
}

func shortenModelName(model string) string {
	return ShortenModelName(model)
}

func generateFallbackRequestID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("nolog_%d_%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b))
}

// ShortenEngineName returns a short identifier for an engine name.
func ShortenEngineName(engine string) string {
	switch engine {
	case EngineGemini:
		return "gm"
	case EngineGPT:
		return "gp"
	case EngineQwen:
		return "qw"
	default:
		return engine
	}
}
