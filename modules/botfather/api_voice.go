package botfather

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/voice"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// Blank import avoided: botfather init runs before voice init (internal/modules.go line 7 vs 23),
// so voice.GetASRLogger() is called dynamically in botTranscribe() rather than in New().

// resolveOwnerAndSpace extracts the owner UID, space ID, and robot ID from the
// bot context (already authenticated by authBot middleware).
// Returns (ownerUID, spaceID, robotID, ok).
func (bf *BotFather) resolveOwnerAndSpace(c *wkhttp.Context) (string, string, string, bool) {
	robot := getRobotFromContext(c)
	if robot == nil {
		bf.Error("invalid bot token: robot not found in context")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"status": http.StatusUnauthorized,
			"msg":    "invalid bot token",
		})
		return "", "", "", false
	}

	robotID := robot.RobotID

	ownerUID := robot.CreatorUID
	if ownerUID == "" {
		bf.Error("bot has no owner", zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("bot has no owner"), http.StatusBadRequest)
		return "", "", "", false
	}

	spaceID, err := bf.db.querySpaceIDByRobotID(robotID)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			bf.Warn("bot is not in any active space", zap.String("robotID", robotID))
			c.ResponseErrorWithStatus(errors.New("bot is not in any active space"), http.StatusBadRequest)
			return "", "", "", false
		}
		bf.Error("query space by robot failed", zap.Error(err), zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("query space failed"), http.StatusInternalServerError)
		return "", "", "", false
	}
	if spaceID == "" {
		bf.Warn("bot is not in any active space", zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("bot is not in any active space"), http.StatusBadRequest)
		return "", "", "", false
	}

	return ownerUID, spaceID, robotID, true
}

// botPutVoiceContext sets the owner's voice correction context (PUT /v1/bot/voice/context)
func (bf *BotFather) botPutVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := bf.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	var req struct {
		Context string `json:"context"`
	}
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorWithStatus(errors.New("invalid request body"), http.StatusBadRequest)
		return
	}

	ctx := strings.TrimSpace(req.Context)

	if ctx == "" {
		c.ResponseErrorWithStatus(errors.New("context cannot be empty"), http.StatusBadRequest)
		return
	}

	if len([]rune(ctx)) > bf.voiceCfg.MaxVoiceContextLength {
		c.ResponseErrorWithStatus(fmt.Errorf("context exceeds max length (%d characters)", bf.voiceCfg.MaxVoiceContextLength), http.StatusBadRequest)
		return
	}

	err := bf.voiceDB.UpsertVoiceContext(ownerUID, spaceID, ctx, robotID)
	if err != nil {
		bf.Error("upsert voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
		c.ResponseErrorWithStatus(errors.New("save voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"msg":    "ok",
	})
}

// botGetVoiceContext queries the owner's voice correction context (GET /v1/bot/voice/context)
func (bf *BotFather) botGetVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := bf.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	m, err := bf.voiceDB.QueryVoiceContext(ownerUID, spaceID)
	if err != nil {
		bf.Error("query voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
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

// botDeleteVoiceContext deletes the owner's voice correction context (DELETE /v1/bot/voice/context)
func (bf *BotFather) botDeleteVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := bf.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	err := bf.voiceDB.DeleteVoiceContext(ownerUID, spaceID)
	if err != nil {
		bf.Error("delete voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
		c.ResponseErrorWithStatus(errors.New("delete voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"msg":    "ok",
	})
}

// botTranscribe handles Bot transcription requests (POST /v1/bot/voice/transcribe)
// Supports per-request mode/model override via form fields.
func (bf *BotFather) botTranscribe(c *wkhttp.Context) {
	if err := bf.voiceCfg.Validate(); err != nil {
		c.ResponseErrorWithStatus(errors.New("voice service not configured"), http.StatusServiceUnavailable)
		return
	}

	file, header, err := c.Request.FormFile("audio")
	if err != nil {
		c.ResponseErrorWithStatus(errors.New("audio file is required"), http.StatusBadRequest)
		return
	}
	defer file.Close()

	if header.Size > bf.voiceCfg.MaxFileSize {
		c.ResponseErrorWithStatus(errors.New("file size exceeds limit"), http.StatusBadRequest)
		return
	}

	audioData, err := io.ReadAll(file)
	if err != nil {
		bf.Error("failed to read audio file", zap.Error(err))
		c.ResponseErrorWithStatus(errors.New("failed to read audio file"), http.StatusInternalServerError)
		return
	}

	mimeType := http.DetectContentType(audioData)
	if mimeType == "application/octet-stream" && header.Header.Get("Content-Type") != "" {
		mimeType = header.Header.Get("Content-Type")
	}

	contextText := c.Request.FormValue("context_text")
	if len([]rune(contextText)) > bf.voiceCfg.MaxContextTextLength {
		contextText = voice.TruncateRunesTail(contextText, bf.voiceCfg.MaxContextTextLength)
	}

	chatContext := c.Request.FormValue("chat_context")
	if len([]rune(chatContext)) > bf.voiceCfg.MaxChatContextLength {
		chatContext = voice.TruncateRunesTail(chatContext, bf.voiceCfg.MaxChatContextLength)
	}

	personalContext := c.Request.FormValue("personal_context")
	if len([]rune(personalContext)) > bf.voiceCfg.MaxVoiceContextLength {
		personalContext = voice.TruncateRunesTail(personalContext, bf.voiceCfg.MaxVoiceContextLength)
	}

	memberContext := c.Request.FormValue("member_context")
	if len([]rune(memberContext)) > bf.voiceCfg.MaxMemberContextLength {
		memberContext = voice.TruncateRunesTail(memberContext, bf.voiceCfg.MaxMemberContextLength)
	}

	// Save original chatContext for ASR logging
	origChatContext := chatContext

	chatContext = voice.BuildVocabularyReference(personalContext, memberContext, chatContext)

	mode := c.Request.FormValue("mode")
	model := c.Request.FormValue("model")

	if mode != "" && mode != "append" && mode != "edit" {
		c.ResponseErrorWithStatus(errors.New("mode must be 'append' or 'edit'"), http.StatusBadRequest)
		return
	}

	effectiveMode := mode
	if effectiveMode == "" {
		effectiveMode = bf.voiceCfg.EditMode
	}
	if bf.voiceCfg.Engine == voice.EngineGPT && effectiveMode == "edit" {
		c.ResponseErrorWithStatus(voice.ErrGPTEditNotSupported, http.StatusBadRequest)
		return
	}

	startTime := time.Now()
	result, err := bf.voiceSvc.TranscribeWithResult(audioData, mimeType, contextText, chatContext,
		voice.TranscribeOptions{Mode: mode, Model: model})
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		bf.Error("transcription failed", zap.Error(err))
		if asrLogger := voice.GetASRLogger(); asrLogger != nil {
			entry := voice.ASREntry{
				RequestID:      asrLogger.GenerateRequestID(),
				Timestamp:      startTime.UTC().Format(time.RFC3339Nano),
				Source:         "bot",
				Engine:         bf.voiceCfg.Engine,
				ModelRequested: model,
				Input: voice.ASRInput{
					Mode:            effectiveMode,
					MimeType:        mimeType,
					AudioSize:       len(audioData),
					ContextText:     contextText,
					ChatContext:     origChatContext,
					PersonalContext: personalContext,
					MemberContext:   memberContext,
					Model:           model,
					Language:        bf.voiceCfg.Language,
				},
				AudioData:  audioData,
				Error:      err.Error(),
				DurationMs: durationMs,
			}
			if result != nil {
				entry.Prompt = &voice.ASRPrompt{
					Type:        result.PromptType,
					Text:        result.PromptText,
					RequestBody: result.RequestBody,
				}
			}
			asrLogger.Enqueue(entry)
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"status": http.StatusInternalServerError,
			"msg":    "transcription failed",
		})
		return
	}

	if asrLogger := voice.GetASRLogger(); asrLogger != nil {
		asrLogger.Enqueue(voice.ASREntry{
			RequestID:      asrLogger.GenerateRequestID(),
			Timestamp:      startTime.UTC().Format(time.RFC3339Nano),
			Source:         "bot",
			Engine:         bf.voiceCfg.Engine,
			ModelRequested: model,
			ModelUsed:      result.Model,
			Input: voice.ASRInput{
				Mode:            effectiveMode,
				MimeType:        mimeType,
				AudioSize:       len(audioData),
				ContextText:     contextText,
				ChatContext:     origChatContext,
				PersonalContext: personalContext,
				MemberContext:   memberContext,
				Model:           model,
				Language:        bf.voiceCfg.Language,
			},
			Prompt: &voice.ASRPrompt{
				Type:        result.PromptType,
				Text:        result.PromptText,
				RequestBody: result.RequestBody,
			},
			AudioData:     audioData,
			RawResultText: result.RawText,
			ResultText:    result.Text,
			ResultLength:  len([]rune(result.Text)),
			IsNoSpeech:    voice.IsNoSpeech(result.RawText),
			DurationMs:    durationMs,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"text":   result.Text,
		"m":      voice.ShortenModelName(result.Model),
		"engine": voice.ShortenEngineName(bf.voiceCfg.Engine),
	})
}
