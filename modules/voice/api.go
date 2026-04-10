package voice

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Voice is the API handler for voice transcription
type Voice struct {
	ctx     *config.Context
	service *VoiceService
	cfg     *VoiceConfig
	db      VoiceStore
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
	if len([]rune(contextText)) > MaxContextTextLength {
		v.Warn("context_text exceeds max length, truncating to keep recent text",
			zap.Int("original_rune_length", len([]rune(contextText))),
			zap.Int("max_length", MaxContextTextLength))
		contextText = TruncateRunesTail(contextText, MaxContextTextLength)
	}

	chatContext := c.Request.FormValue("chat_context")
	if len([]rune(chatContext)) > MaxChatContextLength {
		v.Warn("chat_context exceeds max length, truncating to last characters",
			zap.Int("original_rune_length", len([]rune(chatContext))),
			zap.Int("max_length", MaxChatContextLength))
		chatContext = TruncateRunesTail(chatContext, MaxChatContextLength)
	}

	text, model, err := v.service.Transcribe(audioData, mimeType, contextText, chatContext)
	if err != nil {
		v.Error("transcription failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": http.StatusInternalServerError,
			"msg":    "transcription failed",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"text":   text,
		"m":      shortenModelName(model),
		"engine": shortenEngineName(v.cfg.Engine),
	})
}

// getConfig returns voice feature configuration
func (v *Voice) getConfig(c *wkhttp.Context) {
	enabled := v.cfg.Validate() == nil
	c.JSON(http.StatusOK, gin.H{
		"enabled":       enabled,
		"max_duration":  v.cfg.MaxDuration,
		"max_file_size": v.cfg.MaxFileSize,
		"engine":        shortenEngineName(v.cfg.Engine),
		"edit_mode":     v.cfg.EditMode,
	})
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

func shortenEngineName(engine string) string {
	switch engine {
	case "gemini":
		return "gm"
	case "gpt":
		return "gp"
	default:
		return engine
	}
}
