package voice

import (
	"errors"
	"io"
	"net/http"

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
	log.Log
}

// New creates a new Voice API handler
func New(ctx *config.Context, cfg *VoiceConfig) *Voice {
	return &Voice{
		ctx:     ctx,
		service: NewVoiceService(cfg),
		cfg:     cfg,
		Log:     log.NewTLog("Voice"),
	}
}

// Route registers voice API routes
func (v *Voice) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/voice", v.ctx.AuthMiddleware(r))
	{
		auth.POST("/transcribe", v.transcribe)
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

	chatContext := c.Request.FormValue("chat_context")
	if len(chatContext) > maxChatContextLength {
		v.Warn("chat_context exceeds max length, truncating to last characters",
			zap.Int("original_length", len(chatContext)),
			zap.Int("max_length", maxChatContextLength))
		chatContext = chatContext[len(chatContext)-maxChatContextLength:]
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
	})
}

// getConfig returns voice feature configuration
func (v *Voice) getConfig(c *wkhttp.Context) {
	enabled := v.cfg.Validate() == nil
	c.JSON(http.StatusOK, gin.H{
		"enabled":      enabled,
		"max_duration": v.cfg.MaxDuration,
	})
}

func shortenModelName(model string) string {
	switch model {
	case "gemini-3.1-pro-preview":
		return "g31pp"
	case "gemini-3-flash-preview":
		return "g3fp"
	case "gemini-2.5-pro":
		return "g25p"
	default:
		return model
	}
}
