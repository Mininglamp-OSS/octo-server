package bot_api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/voice_adapter"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

func (ba *BotAPI) resolveOwnerAndSpace(c *wkhttp.Context) (string, string, string, bool) {
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

	spaceID, allSpaces, err := ba.spaceQuerierOrDefault().querySpaceIDsByRobotID(robotID)
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

	if len([]rune(ctx)) > ba.maxVoiceContextLength {
		c.ResponseErrorWithStatus(
			fmt.Errorf("context exceeds max length (%d characters)", ba.maxVoiceContextLength),
			http.StatusBadRequest,
		)
		return
	}

	err := ba.speechClient.PutVocabulary(c.Request.Context(), voice_adapter.PutVocabularyRequest{
		SubjectID: ownerUID,
		ScopeType: "space",
		ScopeID:   spaceID,
		Content:   ctx,
		UpdatedBy: robotID,
	})
	if err != nil {
		ba.Error("put vocabulary failed", zap.Error(err), zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("save voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
}

func (ba *BotAPI) botGetVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := ba.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	vocab, err := ba.speechClient.GetVocabulary(c.Request.Context(), ownerUID, "space", spaceID)
	if err != nil {
		ba.Error("get vocabulary failed", zap.Error(err), zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("query voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":      http.StatusOK,
		"has_context": vocab.HasContent,
		"context":     vocab.Content,
		"updated_at":  vocab.UpdatedAt,
	})
}

func (ba *BotAPI) botDeleteVoiceContext(c *wkhttp.Context) {
	ownerUID, spaceID, robotID, ok := ba.resolveOwnerAndSpace(c)
	if !ok {
		return
	}

	err := ba.speechClient.DeleteVocabulary(c.Request.Context(), ownerUID, "space", spaceID)
	if err != nil {
		ba.Error("delete vocabulary failed", zap.Error(err), zap.String("robotID", robotID))
		c.ResponseErrorWithStatus(errors.New("delete voice context failed"), http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
}

func (ba *BotAPI) botTranscribe(c *wkhttp.Context) {
	botKind := getBotKindFromContext(c)
	if botKind == BotKindApp {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"status": http.StatusForbidden,
			"msg":    "app bot does not support voice operations",
		})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, ba.maxBodySize)

	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.ResponseErrorWithStatus(errors.New("request body too large"), http.StatusRequestEntityTooLarge)
			return
		}
		c.ResponseErrorWithStatus(errors.New("invalid multipart form"), http.StatusBadRequest)
		return
	}
	defer c.Request.MultipartForm.RemoveAll()

	for _, fileHeaders := range c.Request.MultipartForm.File {
		for _, fh := range fileHeaders {
			if fh.Size > ba.maxFileSize {
				c.ResponseErrorWithStatus(
					fmt.Errorf("file %s exceeds max size %d bytes", fh.Filename, ba.maxFileSize),
					http.StatusRequestEntityTooLarge,
				)
				return
			}
		}
	}

	mode := c.Request.FormValue("mode")
	switch mode {
	case "append":
		mode = "append_only"
	case "edit", "":
		mode = "smart"
	default:
		c.ResponseErrorWithStatus(
			fmt.Errorf("invalid mode '%s': must be 'append' or 'edit'", mode),
			http.StatusBadRequest,
		)
		return
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	modeWritten := false
	for key, vals := range c.Request.MultipartForm.Value {
		v := vals[0]
		if key == "mode" {
			v = mode
			modeWritten = true
		}
		if err := w.WriteField(key, v); err != nil {
			c.ResponseErrorWithStatus(errors.New("failed to build request"), http.StatusInternalServerError)
			return
		}
	}
	if !modeWritten {
		if err := w.WriteField("mode", mode); err != nil {
			c.ResponseErrorWithStatus(errors.New("failed to build request"), http.StatusInternalServerError)
			return
		}
	}

	for key, fileHeaders := range c.Request.MultipartForm.File {
		for _, fh := range fileHeaders {
			src, err := fh.Open()
			if err != nil {
				c.ResponseErrorWithStatus(errors.New("failed to read uploaded file"), http.StatusBadRequest)
				return
			}

			partHeader := make(textproto.MIMEHeader)
			partHeader.Set("Content-Disposition",
				fmt.Sprintf(`form-data; name="%s"; filename="%s"`, key, fh.Filename))
			ct := fh.Header.Get("Content-Type")
			if ct == "" {
				ct = "application/octet-stream"
			}
			partHeader.Set("Content-Type", ct)

			dst, err := w.CreatePart(partHeader)
			if err != nil {
				src.Close()
				c.ResponseErrorWithStatus(errors.New("failed to build request"), http.StatusInternalServerError)
				return
			}
			if _, err := io.Copy(dst, src); err != nil {
				src.Close()
				c.ResponseErrorWithStatus(errors.New("failed to build request"), http.StatusInternalServerError)
				return
			}
			src.Close()
		}
	}
	w.Close()

	resp, err := ba.speechClient.ForwardTranscribeBody(c.Request.Context(), &buf, w.FormDataContentType())
	if err != nil {
		ba.Error("forward transcribe failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{
			"status": http.StatusBadGateway,
			"msg":    "speech service unavailable",
		})
		return
	}
	defer resp.Body.Close()
	c.DataFromReader(resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Type"), resp.Body, nil)
}
