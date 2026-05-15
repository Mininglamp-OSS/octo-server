package notify

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// InternalTokenHeader is the header key for internal service authentication.
const InternalTokenHeader = "X-Internal-Token"

// Notify 通知模块
type Notify struct {
	ctx           *config.Context
	userService   user.IService
	appService    app.IService
	db            *dbr.Session
	memberCache   *memberCache
	botMu         sync.Mutex
	botOK         bool
	internalToken string
	log.Log
}

// New 创建 Notify 实例
func New(ctx *config.Context) *Notify {
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	if token == "" {
		log.NewTLog("Notify").Warn("NOTIFY_INTERNAL_TOKEN not set — internal API will reject all requests")
	}

	n := &Notify{
		ctx:           ctx,
		userService:   user.NewService(ctx),
		appService:    app.NewService(ctx),
		db:            ctx.DB(),
		memberCache:   newMemberCache(),
		internalToken: token,
		Log:           log.NewTLog("Notify"),
	}

	// 注册缓存失效回调（通过 event 包避免循环依赖）
	event.SpaceMemberCacheInvalidator = func(spaceID string) {
		n.memberCache.invalidate(spaceID)
	}

	// Static bot: no per-Space provisioning needed
	event.NotifyBotProvisioner = func(spaceID string, spaceName string) {
		// no-op: notification bot is a global singleton
	}

	// 监听成员加入事件
	ctx.AddEventListener(event.SpaceMemberJoin, n.handleSpaceMemberEvent)

	// 启动时创建全局通知 Bot（单例，带 panic recovery）
	go func() {
		defer func() {
			if r := recover(); r != nil {
				n.Error("ensureNotifyBot panic", zap.Any("recover", r))
			}
		}()
		n.botMu.Lock()
		if !n.botOK {
			n.botOK = n.ensureNotifyBot()
		}
		n.botMu.Unlock()
		if n.botOK {
			n.Info("Notify bot ready")
		}
	}()

	return n
}

// Route 路由配置
func (n *Notify) Route(r *wkhttp.WKHttp) {
	internal := r.Group("/v1/internal", n.internalAuthMiddleware())
	{
		internal.POST("/notify", n.sendNotify)
		internal.POST("/notify/batch", n.sendNotifyBatch)
	}
}

// internalAuthMiddleware 内部服务认证中间件。
// token 未配置时 fail-closed（拒绝所有请求）。
func (n *Notify) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if n.internalToken == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{"error": "internal API auth not configured"})
			return
		}
		token := c.GetHeader(InternalTokenHeader)
		if subtle.ConstantTimeCompare([]byte(token), []byte(n.internalToken)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// handleSpaceMemberEvent 成员变动时失效缓存
func (n *Notify) handleSpaceMemberEvent(data []byte, commit config.EventCommit) {
	var req map[string]interface{}
	if err := util.ReadJsonByByte(data, &req); err != nil {
		n.Warn("解析SpaceMember事件失败", zap.Error(err))
		commit(nil)
		return
	}
	if spaceID, _ := req["space_id"].(string); spaceID != "" {
		n.memberCache.invalidate(spaceID)
	}
	commit(nil)
}

// sendNotify handles POST /v1/internal/notify
func (n *Notify) sendNotify(c *wkhttp.Context) {
	var req NotifyReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorWithStatus(errors.New("参数格式错误"), http.StatusBadRequest)
		return
	}

	resp, err := n.deliverNotification(&req)
	if err != nil {
		n.Error("投递通知失败", zap.Error(err), zap.String("space_id", req.SpaceID))
		c.ResponseErrorWithStatus(errors.New("internal error"), http.StatusInternalServerError)
		return
	}
	c.Response(resp)
}

// sendNotifyBatch handles POST /v1/internal/notify/batch
func (n *Notify) sendNotifyBatch(c *wkhttp.Context) {
	var req BatchNotifyReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorWithStatus(errors.New("参数格式错误"), http.StatusBadRequest)
		return
	}
	if len(req.Notifications) == 0 {
		c.ResponseErrorWithStatus(errors.New("notifications不能为空"), http.StatusBadRequest)
		return
	}
	if len(req.Notifications) > 50 {
		c.ResponseErrorWithStatus(errors.New("批量上限50条"), http.StatusBadRequest)
		return
	}

	hasErrors := false
	results := make([]BatchNotifyResult, 0, len(req.Notifications))
	for i := range req.Notifications {
		resp, err := n.deliverNotification(&req.Notifications[i])
		if err != nil {
			n.Error("批量投递通知失败", zap.Error(err), zap.Int("index", i))
			hasErrors = true
			results = append(results, BatchNotifyResult{
				NotifyResp: NotifyResp{Delivered: []string{}, Filtered: map[string]string{}},
				Error:      err.Error(),
			})
			continue
		}
		results = append(results, BatchNotifyResult{NotifyResp: *resp})
	}

	resp := &BatchNotifyResp{Results: results, HasErrors: hasErrors}
	if hasErrors {
		c.JSON(http.StatusMultiStatus, resp)
	} else {
		c.Response(resp)
	}
}

// deliverNotification 校验、过滤、投递
func (n *Notify) deliverNotification(req *NotifyReq) (*NotifyResp, error) {
	if req.SpaceID == "" {
		return nil, errors.New("space_id不能为空")
	}
	if len(req.Targets) == 0 {
		return nil, errors.New("targets不能为空")
	}
	if len(req.Targets) > 200 {
		return nil, errors.New("targets上限200")
	}
	if req.Payload == nil {
		return nil, errors.New("payload不能为空")
	}

	// 去重 + 排除 actor
	targets := dedupTargets(req.Targets)
	if req.ActorUID != "" {
		tmp := make([]string, 0, len(targets))
		for _, uid := range targets {
			if uid != req.ActorUID {
				tmp = append(tmp, uid)
			}
		}
		targets = tmp
	}

	// 成员校验（B3 修复：先 refresh 缓存，再从缓存过滤，单次 DB 查询）
	members, filteredMap, err := n.memberCache.verify(n.db, req.SpaceID, targets)
	if err != nil {
		return nil, fmt.Errorf("member verification failed: %w", err)
	}

	if len(members) == 0 {
		return &NotifyResp{
			Delivered: []string{},
			Filtered:  filteredMap,
		}, nil
	}

	// 确保 Bot 存在（失败可重试，不用 sync.Once）
	if !n.botOK {
		n.botMu.Lock()
		if !n.botOK {
			n.botOK = n.ensureNotifyBot()
		}
		n.botMu.Unlock()
	}
	if !n.botOK {
		return nil, errors.New("notify bot unavailable")
	}

	// Clone to avoid mutating caller's map. The PERSONAL builder
	// (NewPersonalMsgSendReq) is the single authority for payload.space_id,
	// so the legacy "if absent, inject req.SpaceID" check that lived here
	// (mirrored from botfather/command.go:951) is removed in YUJ-674 /
	// Mininglamp-OSS#37 — the builder either overrides with req.SpaceID or
	// strips on empty, both fail-closed.
	payload := make(map[string]interface{}, len(req.Payload))
	for k, v := range req.Payload {
		payload[k] = v
	}

	// 并发投递（bounded worker pool，最多 20 并发）
	fromUID := NotifyBotUID()

	type sendResult struct {
		uid     string
		success bool
	}
	resultCh := make(chan sendResult, len(members))
	sem := make(chan struct{}, 20)

	for _, targetUID := range members {
		sem <- struct{}{}
		go func(uid string) {
			defer func() { <-sem }()
			// Per-goroutine map clone is required: NewPersonalMsgSendReq mutates
			// the payload map (sets/strips space_id) before encoding, so a shared
			// map across goroutines would race.
			perCall := make(map[string]interface{}, len(payload))
			for k, v := range payload {
				perCall[k] = v
			}
			err := n.ctx.SendMessage(config.NewPersonalMsgSendReq(
				uid,
				fromUID,
				perCall,
				req.SpaceID,
				config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
			))
			if err != nil {
				n.Warn("发送通知消息失败",
					zap.String("target", uid),
					zap.String("space_id", req.SpaceID),
					zap.Error(err))
			}
			resultCh <- sendResult{uid: uid, success: err == nil}
		}(targetUID)
	}

	delivered := make([]string, 0, len(members))
	for range members {
		r := <-resultCh
		if r.success {
			delivered = append(delivered, r.uid)
		} else {
			filteredMap[r.uid] = "send_failed"
		}
	}

	n.Info("notify_delivered",
		zap.String("service", req.Service),
		zap.String("space_id", req.SpaceID),
		zap.String("event", req.Event),
		zap.Int("targets", len(req.Targets)),
		zap.Int("delivered", len(delivered)),
		zap.Int("filtered", len(filteredMap)),
	)

	return &NotifyResp{
		Delivered: delivered,
		Filtered:  filteredMap,
	}, nil
}

// dedupTargets 去重并保持顺序
func dedupTargets(targets []string) []string {
	seen := make(map[string]bool, len(targets))
	result := make([]string, 0, len(targets))
	for _, uid := range targets {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		result = append(result, uid)
	}
	return result
}
