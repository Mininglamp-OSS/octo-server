package notification

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
	"go.uber.org/zap"
)

type Service struct {
	ctx *config.Context
	db  *dbStore
	log.Log
}

const maxPauseDuration = 30 * 24 * time.Hour

func validPauseUntil(now, pausedUntil time.Time) bool {
	return pausedUntil.After(now) && !pausedUntil.After(now.Add(maxPauseDuration))
}

func New(ctx *config.Context) *Service {
	return &Service{ctx: ctx, db: newDBStore(ctx), Log: log.NewTLog("Notification")}
}

func (s *Service) Route(r *wkhttp.WKHttp) {
	user := r.Group("/v1/user", s.ctx.AuthMiddleware(r), appwkhttp.SharedUIDRateLimiter(r, s.ctx))
	{
		user.GET("/notification-pause", s.getPause)
		user.PUT("/notification-pause", s.putPause)
		user.DELETE("/notification-pause", s.deletePause)
	}
}

func (s *Service) getPause(c *wkhttp.Context) {
	now := time.Now().UTC()
	record, err := s.db.get(c.GetLoginUID())
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	c.Response(s.response(record, now))
}

func (s *Service) putPause(c *wkhttp.Context) {
	var req updatePauseRequest
	if err := c.BindJSON(&req); err != nil {
		s.writeInvalidTime(c)
		return
	}
	now := time.Now().UTC()
	pausedUntil := req.PausedUntil.UTC()
	if !validPauseUntil(now, pausedUntil) {
		s.writeInvalidTime(c)
		return
	}
	record, err := s.db.upsert(c.GetLoginUID(), pausedUntil, now)
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	response := s.response(record, now)
	if err := s.sendChangedCMD(c.GetLoginUID(), response); err != nil {
		s.Warn("发送通知暂停状态 CMD 失败", zap.String("uid", c.GetLoginUID()), zap.Error(err))
	}
	c.Response(response)
}

func (s *Service) deletePause(c *wkhttp.Context) {
	now := time.Now().UTC()
	record, err := s.db.clear(c.GetLoginUID(), now)
	if err != nil {
		s.writeStoreError(c, err)
		return
	}
	response := s.response(record, now)
	if err := s.sendChangedCMD(c.GetLoginUID(), response); err != nil {
		s.Warn("发送通知暂停状态 CMD 失败", zap.String("uid", c.GetLoginUID()), zap.Error(err))
	}
	c.Response(response)
}

func (s *Service) response(record *pauseRecord, now time.Time) pauseResponse {
	response := pauseResponse{ServerTime: now.UTC()}
	if record == nil {
		return response
	}
	response.Revision = record.Revision
	if record.PausedUntil != nil && record.PausedUntil.After(now) {
		response.Paused = true
		until := record.PausedUntil.UTC()
		response.PausedUntil = &until
	}
	return response
}

func (s *Service) sendChangedCMD(uid string, response pauseResponse) error {
	return s.ctx.SendCMD(config.MsgCMDReq{
		NoPersist:   true,
		ChannelID:   uid,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         notificationPauseCMD,
		Param: map[string]interface{}{
			"paused":       response.Paused,
			"paused_until": response.PausedUntil,
			"revision":     response.Revision,
			"server_time":  response.ServerTime,
		},
	})
}

func (s *Service) writeInvalidTime(c *wkhttp.Context) {
	respondInvalidPauseTime(c)
}

func (s *Service) writeStoreError(c *wkhttp.Context, err error) {
	s.Error("通知暂停状态存储失败", zap.String("uid", c.GetLoginUID()), zap.Error(err))
	respondPauseUpdateFailed(c)
}

// ActiveUIDs returns the account-level pause state for a batch of recipients.
// It is intentionally read-only so webhook can fail closed without sharing API concerns.
func (s *Service) ActiveUIDs(uids []string, now time.Time) (map[string]struct{}, error) {
	return s.db.getActiveByUIDs(uids, now)
}
