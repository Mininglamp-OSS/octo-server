package bot_api

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

// resolveOwnerAndSpace extracts owner UID, space ID, and robot ID from bot context.
// Returns (ownerUID, spaceID, robotID, ok).
func (ba *BotAPI) resolveOwnerAndSpace(c *wkhttp.Context) (string, string, string, bool) {
	// Voice API is User Bot only
	botKind := getBotKindFromContext(c)
	if botKind == BotKindApp {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"status": http.StatusForbidden,
			"msg":    "app bot does not support voice operations",
		})
		return "", "", "", false
	}

	robot := getRobotFromContext(c)
	if robot == nil {
		ba.Error("invalid bot token: robot not found in context")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"status": http.StatusUnauthorized,
			"msg":    "invalid bot token",
		})
		return "", "", "", false
	}

	robotID := robot.RobotID
	ownerUID := robot.CreatorUID
	if ownerUID == "" {
		ba.Error("bot has no owner", zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("bot has no owner"), http.StatusBadRequest)
		return "", "", "", false
	}

	// YUJ-688 medium ask: Voice context path was deterministic but used the
	// single-row wrapper, discarding the multi-Space list and never emitting
	// the `multi_space_membership=true` observability signal. Switch to the
	// multi-row variant so multi-Space User Bot voice traffic is visible to
	// operators alongside the bot_api / robot dispatchers.
	spaceID, allSpaces, err := ba.db.querySpaceIDsByRobotID(robotID)
	if err != nil {
		if errors.Is(err, dbr.ErrNotFound) {
			ba.Warn("bot is not in any active space", zap.String("robotID", robotID))
			c.ResponseErrorWithStatus(errors.New("bot is not in any active space"), http.StatusBadRequest)
			return "", "", "", false
		}
		ba.Error("query space by robot failed", zap.Error(err), zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("query space failed"), http.StatusInternalServerError)
		return "", "", "", false
	}
	if spaceID == "" {
		ba.Warn("bot is not in any active space", zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("bot is not in any active space"), http.StatusBadRequest)
		return "", "", "", false
	}
	if len(allSpaces) > 1 {
		// Mirror the dispatch-path warn shape so a single SLO query covers all
		// multi-Space dispatch entry points (bot_api send, robot send, voice).
		ba.Warn("multi_space_membership",
			zap.Bool("multi_space_membership", true),
			zap.String("dispatcher", "bot_api_voice"),
			zap.String("robotID", robotID),
			zap.String("chosen_space_id", spaceID),
			zap.Strings("all_space_ids", allSpaces),
		)
	}

	return ownerUID, spaceID, robotID, true
}

// botPutVoiceContext handles PUT /v1/bot/voice/context.
func (ba *BotAPI) botPutVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := ba.resolveOwnerAndSpace(c)
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

	if len([]rune(ctx)) > ba.voiceCfg.MaxVoiceContextLength {
		c.ResponseErrorWithStatus(fmt.Errorf("context exceeds max length (%d characters)", ba.voiceCfg.MaxVoiceContextLength), http.StatusBadRequest)
		return
	}

	err := ba.voiceDB.UpsertVoiceContext(ownerUID, spaceID, ctx, robotID)
	if err != nil {
		ba.Error("upsert voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
		c.ResponseErrorWithStatus(errors.New("save voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"msg":    "ok",
	})
}

// botGetVoiceContext handles GET /v1/bot/voice/context.
func (ba *BotAPI) botGetVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := ba.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	m, err := ba.voiceDB.QueryVoiceContext(ownerUID, spaceID)
	if err != nil {
		ba.Error("query voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
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

// botDeleteVoiceContext handles DELETE /v1/bot/voice/context.
func (ba *BotAPI) botDeleteVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := ba.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	err := ba.voiceDB.DeleteVoiceContext(ownerUID, spaceID)
	if err != nil {
		ba.Error("delete voice context failed", zap.Error(err), zap.String("robotID", robotID), zap.String("ownerUID", ownerUID))
		c.ResponseErrorWithStatus(errors.New("delete voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": http.StatusOK,
		"msg":    "ok",
	})
}

// botTranscribe handles POST /v1/bot/voice/transcribe.
func (ba *BotAPI) botTranscribe(c *wkhttp.Context) {
	// Voice guard: App Bot not allowed
	botKind := getBotKindFromContext(c)
	if botKind == BotKindApp {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"status": http.StatusForbidden,
			"msg":    "app bot does not support voice operations",
		})
		return
	}

	if err := ba.voiceCfg.Validate(); err != nil {
		c.ResponseErrorWithStatus(errors.New("voice service not configured"), http.StatusServiceUnavailable)
		return
	}

	file, header, err := c.Request.FormFile("audio")
	if err != nil {
		c.ResponseErrorWithStatus(errors.New("audio file is required"), http.StatusBadRequest)
		return
	}
	defer file.Close()

	if header.Size > ba.voiceCfg.MaxFileSize {
		c.ResponseErrorWithStatus(errors.New("file size exceeds limit"), http.StatusBadRequest)
		return
	}

	audioData, err := io.ReadAll(file)
	if err != nil {
		ba.Error("failed to read audio file", zap.Error(err))
		c.ResponseErrorWithStatus(errors.New("failed to read audio file"), http.StatusInternalServerError)
		return
	}

	mimeType := http.DetectContentType(audioData)
	if mimeType == "application/octet-stream" && header.Header.Get("Content-Type") != "" {
		mimeType = header.Header.Get("Content-Type")
	}

	contextText := c.Request.FormValue("context_text")
	if len([]rune(contextText)) > ba.voiceCfg.MaxContextTextLength {
		contextText = voice.TruncateRunesTail(contextText, ba.voiceCfg.MaxContextTextLength)
	}

	chatContext := c.Request.FormValue("chat_context")
	if len([]rune(chatContext)) > ba.voiceCfg.MaxChatContextLength {
		chatContext = voice.TruncateRunesTail(chatContext, ba.voiceCfg.MaxChatContextLength)
	}

	personalContext := c.Request.FormValue("personal_context")
	if len([]rune(personalContext)) > ba.voiceCfg.MaxVoiceContextLength {
		personalContext = voice.TruncateRunesTail(personalContext, ba.voiceCfg.MaxVoiceContextLength)
	}

	memberContext := c.Request.FormValue("member_context")
	if len([]rune(memberContext)) > ba.voiceCfg.MaxMemberContextLength {
		memberContext = voice.TruncateRunesTail(memberContext, ba.voiceCfg.MaxMemberContextLength)
	}

	channelType := c.Request.FormValue("channel_type")
	if channelType == "" {
		channelType = "2"
	}
	skipMention := channelType == "1"
	if skipMention {
		memberContext = ""
	}

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
		effectiveMode = ba.voiceCfg.EditMode
	}
	if ba.voiceCfg.Engine == voice.EngineGPT && effectiveMode == "edit" {
		c.ResponseErrorWithStatus(voice.ErrGPTEditNotSupported, http.StatusBadRequest)
		return
	}

	startTime := time.Now()
	result, err := ba.voiceSvc.TranscribeWithResult(audioData, mimeType, contextText, chatContext,
		voice.TranscribeOptions{Mode: mode, Model: model, SkipMention: skipMention})
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		ba.Error("transcription failed", zap.Error(err))
		if asrLogger := voice.GetASRLogger(); asrLogger != nil {
			entry := voice.ASREntry{
				RequestID:      asrLogger.GenerateRequestID(),
				Timestamp:      startTime.UTC().Format(time.RFC3339Nano),
				Source:         "bot",
				Engine:         ba.voiceCfg.Engine,
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
					Language:        ba.voiceCfg.Language,
					ChannelType:     channelType,
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
			Engine:         ba.voiceCfg.Engine,
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
				Language:        ba.voiceCfg.Language,
				ChannelType:     channelType,
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
		"engine": voice.ShortenEngineName(ba.voiceCfg.Engine),
	})
}
