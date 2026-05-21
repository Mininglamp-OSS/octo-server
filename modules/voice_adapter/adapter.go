package voice_adapter

import (
	"errors"
	"net/http"

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
	}
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
	resp, err := a.client.GetConfig(c.Request.Context())
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
	c.JSON(http.StatusOK, resp)
}

func (a *VoiceAdapter) getContext(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseErrorWithStatus(errors.New("space_id is required"), http.StatusBadRequest)
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
