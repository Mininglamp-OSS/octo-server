package voice_adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type VoiceAdapter struct {
	ctx    *config.Context
	client *SpeechClient
	cfg    *AdapterConfig
	log.Log
}

func NewVoiceAdapter(ctx *config.Context, cfg *AdapterConfig) *VoiceAdapter {
	return &VoiceAdapter{
		ctx:    ctx,
		client: NewSpeechClient(cfg.SpeechServiceURL, cfg.SpeechAPIKey, cfg.SpeechTimeout),
		cfg:    cfg,
		Log:    log.NewTLog("VoiceAdapter"),
	}
}

func (a *VoiceAdapter) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/voice", a.ctx.AuthMiddleware(r))
	{
		auth.POST("/transcribe", a.transcribe)
		auth.GET("/config", a.getConfig)
		auth.GET("/context", a.getContext)
		auth.GET("/document/asr_service_doc", a.getDocument)
		auth.PUT("/local-config", a.putLocalConfig)
		auth.GET("/local-config", a.getLocalConfig)
		auth.DELETE("/local-config", a.deleteLocalConfig)
		auth.POST("/local-config/reset", a.resetLocalConfig)
	}
}

func getSpaceID(c *wkhttp.Context) string {
	return c.GetHeader("X-Space-ID")
}

func (a *VoiceAdapter) transcribe(c *wkhttp.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, a.cfg.MaxBodySize)

	resp, err := a.client.ForwardTranscribe(c.Request)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"status": http.StatusRequestEntityTooLarge,
				"msg":    "request body too large",
			})
			return
		}
		a.Error("forward transcribe failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{
			"status": http.StatusBadGateway,
			"msg":    "speech service unavailable",
		})
		return
	}
	defer resp.Body.Close()
	c.DataFromReader(resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Type"), resp.Body, nil)
}

func (a *VoiceAdapter) getConfig(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "msg": "check space membership failed"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"status": http.StatusForbidden, "msg": "not a member of this space"})
		return
	}

	scopeType := "space"
	scopeID := spaceID

	resp, err := a.client.GetConfig(c.Request.Context(), loginUID, scopeType, scopeID)
	if err != nil {
		var svcErr *SpeechServiceError
		if errors.As(err, &svcErr) && (svcErr.StatusCode == 401 || svcErr.StatusCode == 403) {
			a.Error("speech service auth failure", zap.Int("status", svcErr.StatusCode), zap.Error(err))
			c.JSON(http.StatusBadGateway, gin.H{
				"status": http.StatusBadGateway,
				"msg":    "speech service configuration error",
			})
			return
		}
		a.Warn("get config failed, returning disabled fallback", zap.Error(err))
		c.JSON(http.StatusOK, gin.H{
			"enabled": false,
		})
		return
	}
	if a.cfg.FeedbackPrivacyURL != "" {
		resp["feedback_privacy_url"] = a.cfg.FeedbackPrivacyURL
	}
	if a.cfg.UserAgreementURL != "" {
		resp["feedback_user_agreement_url"] = a.cfg.UserAgreementURL
	}
	c.JSON(http.StatusOK, resp)
}

func (a *VoiceAdapter) getContext(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.ResponseErrorWithStatus(errors.New("check space membership failed"), http.StatusInternalServerError)
		return
	}
	if !isMember {
		c.ResponseErrorWithStatus(errors.New("no permission to access this space"), http.StatusForbidden)
		return
	}

	vocab, err := a.client.GetVocabulary(c.Request.Context(), loginUID, "space", spaceID)
	if err != nil {
		a.Error("get vocabulary failed", zap.Error(err))
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

func (a *VoiceAdapter) getDocument(c *wkhttp.Context) {
	docPath := a.cfg.ASRServiceDocFile
	if docPath == "" {
		docPath = "./assets/web/asr_service_doc.html"
	}

	content, err := os.ReadFile(docPath)
	if err != nil {
		a.Error("read asr service doc failed", zap.String("path", docPath), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"msg": "document not available"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"doc_type":   "asr_service_doc",
		"title":      "Octo 语音转写服务说明",
		"content":    string(content),
		"version":    "2.0",
		"updated_at": "2026-05-25",
	})
}

func (a *VoiceAdapter) putLocalConfig(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)

	var req struct {
		Enabled       *bool   `json:"enabled"`
		TimeoutMs     *int    `json:"timeout_ms"`
		ProbeURL      *string `json:"probe_url"`
		TranscribeURL *string `json:"transcribe_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"status": http.StatusRequestEntityTooLarge, "msg": "request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "invalid request body"})
		return
	}

	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	if req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "enabled is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "msg": "check space membership failed"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"status": http.StatusForbidden, "msg": "not a member of this space"})
		return
	}

	err = a.client.PutLocalConfig(c.Request.Context(), loginUID, "space", spaceID, *req.Enabled, req.TimeoutMs, req.ProbeURL, req.TranscribeURL)
	if err != nil {
		var svcErr *SpeechServiceError
		if errors.As(err, &svcErr) && svcErr.StatusCode >= 400 && svcErr.StatusCode < 500 {
			c.JSON(svcErr.StatusCode, gin.H{"status": svcErr.StatusCode, "msg": svcErr.Body})
			return
		}
		a.Error("put local config failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"status": http.StatusBadGateway, "msg": "speech service unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
}

func (a *VoiceAdapter) getLocalConfig(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "msg": "check space membership failed"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"status": http.StatusForbidden, "msg": "not a member of this space"})
		return
	}

	resp, err := a.client.GetLocalConfig(c.Request.Context(), loginUID, "space", spaceID)
	if err != nil {
		var svcErr *SpeechServiceError
		if errors.As(err, &svcErr) && svcErr.StatusCode >= 400 && svcErr.StatusCode < 500 {
			c.JSON(svcErr.StatusCode, gin.H{"status": svcErr.StatusCode, "msg": svcErr.Body})
			return
		}
		a.Error("get local config failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"status": http.StatusBadGateway, "msg": "speech service unavailable"})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (a *VoiceAdapter) resetLocalConfig(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)

	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"status": http.StatusRequestEntityTooLarge, "msg": "request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "invalid request body"})
		return
	}

	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	if req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "enabled is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "msg": "check space membership failed"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"status": http.StatusForbidden, "msg": "not a member of this space"})
		return
	}

	err = a.client.ResetLocalConfig(c.Request.Context(), loginUID, "space", spaceID, *req.Enabled)
	if err != nil {
		var svcErr *SpeechServiceError
		if errors.As(err, &svcErr) && svcErr.StatusCode >= 400 && svcErr.StatusCode < 500 {
			c.JSON(svcErr.StatusCode, gin.H{"status": svcErr.StatusCode, "msg": svcErr.Body})
			return
		}
		a.Error("reset local config failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"status": http.StatusBadGateway, "msg": "speech service unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
}

func (a *VoiceAdapter) deleteLocalConfig(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	spaceID := getSpaceID(c)
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": http.StatusBadRequest, "msg": "X-Space-ID header is required"})
		return
	}

	isMember, err := space.CheckMembership(a.ctx.DB(), spaceID, loginUID)
	if err != nil {
		a.Error("check space membership failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"status": http.StatusInternalServerError, "msg": "check space membership failed"})
		return
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"status": http.StatusForbidden, "msg": "not a member of this space"})
		return
	}

	err = a.client.DeleteLocalConfig(c.Request.Context(), loginUID, "space", spaceID)
	if err != nil {
		var svcErr *SpeechServiceError
		if errors.As(err, &svcErr) {
			if svcErr.StatusCode == 404 {
				var parsed struct {
					Status int `json:"status"`
				}
				if json.Unmarshal([]byte(svcErr.Body), &parsed) == nil && parsed.Status == 404 {
					c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
					return
				}
			}
			if svcErr.StatusCode >= 400 && svcErr.StatusCode < 500 {
				c.JSON(svcErr.StatusCode, gin.H{"status": svcErr.StatusCode, "msg": svcErr.Body})
				return
			}
		}
		a.Error("delete local config failed", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"status": http.StatusBadGateway, "msg": "speech service unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": http.StatusOK, "msg": "ok"})
}
