package notify

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/base/event"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/gocraft/dbr/v2"
	"go.uber.org/zap"
)

// InternalTokenHeader is the header key for internal service authentication.
const InternalTokenHeader = "X-Internal-Token"

const notifyCapabilityContextKey = "octo.notify.capability"

type notifyCapabilityKind string

const (
	notifyCapabilityLegacy notifyCapabilityKind = "legacy"
	notifyCapabilityDocs   notifyCapabilityKind = "docs"
	notifyCapabilityAction notifyCapabilityKind = "action"
)

type notifyCapability struct {
	Kind   notifyCapabilityKind
	Action cardactiondispatch.NotifyCapability
}

// These carddispatch producers are bound to the shared `notification` User Bot
// so summary cards, docs cards, generic action outcomes, and legacy text
// notifications appear in one system DM conversation. Capability isolation
// lives at the producer level.
const (
	summaryNotifyProducerID = "summary-notify"
	docsNotifyProducerID    = "docs-notify"
	actionOutcomeProducerID = "action-outcome"
)

var (
	errNotifyCardNotAllowed = errors.New("card payload not allowed on internal notify ingress")
	errNotifyCardInvalid    = errors.New("card notification request is invalid")
)

// Notify 通知模块
type Notify struct {
	ctx           *config.Context
	userService   user.IService
	appService    app.IService
	db            *dbr.Session
	memberCache   *memberCache
	botMu         sync.Mutex
	botOK         atomic.Bool
	cardSender    carddispatch.Sender
	docsSender    carddispatch.Sender
	actionService *cardactiondispatch.Service
	actionSenders map[cardactiondispatch.NotifyCapability]carddispatch.Sender
	internalToken string
	docsToken     string
	spaceWelcome  *spaceWelcomeService
	log.Log
}

// New 创建 Notify 实例
func New(ctx *config.Context) *Notify {
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	docsToken := os.Getenv("OCTO_DOCS_NOTIFY_TOKEN")
	if token == "" {
		log.NewTLog("Notify").Warn("NOTIFY_INTERNAL_TOKEN not set — internal API will reject all requests")
	}
	if docsToken == "" {
		log.NewTLog("Notify").Warn("OCTO_DOCS_NOTIFY_TOKEN not set — docs notification requests will be rejected")
	}
	if token != "" && docsToken == token {
		log.NewTLog("Notify").Error("OCTO_DOCS_NOTIFY_TOKEN must differ from NOTIFY_INTERNAL_TOKEN; docs capability disabled")
		docsToken = ""
	}

	n := &Notify{
		ctx:           ctx,
		userService:   user.NewService(ctx),
		appService:    app.NewService(ctx),
		db:            ctx.DB(),
		memberCache:   newMemberCache(),
		internalToken: token,
		docsToken:     docsToken,
		Log:           log.NewTLog("Notify"),
		actionSenders: make(map[cardactiondispatch.NotifyCapability]carddispatch.Sender),
	}

	// Obtain the producer-bound card Senders from the single registry composed at
	// bootstrap (main.installCardDispatch, before module construction). A missing
	// registration is non-fatal: card notifications degrade to the text DM path.
	if sender, senderErr := carddispatch.SenderFromContext(ctx, summaryNotifyProducerID); senderErr != nil {
		n.Warn("summary-notify card sender unavailable; card notifications will degrade to text", zap.Error(senderErr))
	} else {
		n.cardSender = sender
	}
	if sender, senderErr := carddispatch.SenderFromContext(ctx, docsNotifyProducerID); senderErr != nil {
		n.Warn("docs-notify card sender unavailable; docs card notifications will degrade to text", zap.Error(senderErr))
	} else {
		n.docsSender = sender
	}
	if actionService, ok := cardactiondispatch.FromContext(ctx); ok {
		n.actionService = actionService
		for _, capability := range actionService.NotifyProducers() {
			sender, senderErr := carddispatch.SenderFromContext(ctx, ActionNotifyProducerID(capability.Owner))
			if senderErr != nil {
				n.Error("action-notify card sender unavailable", zap.String("owner", capability.Owner), zap.Error(senderErr))
				continue
			}
			n.actionSenders[capability] = sender
		}
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

	// Space 新成员欢迎语（task space-new-user-welcome-message）。欢迎语是所有人
	// 同一份纯文本（不区分语言）。服务对象在此构造，reconciler/worker goroutine
	// 由模块 Start() 启动、Stop() 停止。
	n.spaceWelcome = newSpaceWelcomeService(
		ctx,
		common.EnsureSystemSettings(ctx),
		NotifyBotUID(),
		func() bool {
			n.ensureNotifyBotReady()
			return n.botOK.Load()
		},
	)

	return n
}

// ensureNotifyBotReady provisions the shared notification bot on demand
// (idempotent, retriable). Legacy text notifications, summary cards, and their
// text fallback all use this one DM identity.
func (n *Notify) ensureNotifyBotReady() {
	if n.botOK.Load() {
		return
	}
	n.botMu.Lock()
	if !n.botOK.Load() {
		n.botOK.Store(n.ensureNotifyBot())
	}
	n.botMu.Unlock()
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
		if n.internalToken == "" && n.docsToken == "" && n.actionService == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{"error": "internal API auth not configured"})
			return
		}
		token := c.GetHeader(InternalTokenHeader)
		switch {
		case n.internalToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(n.internalToken)) == 1:
			c.Set(notifyCapabilityContextKey, notifyCapability{Kind: notifyCapabilityLegacy})
		case n.docsToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(n.docsToken)) == 1:
			c.Set(notifyCapabilityContextKey, notifyCapability{Kind: notifyCapabilityDocs})
		default:
			if action, ok := n.actionService.ResolveNotifyToken(token); ok {
				c.Set(notifyCapabilityContextKey, notifyCapability{Kind: notifyCapabilityAction, Action: action})
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

// handleSpaceMemberEvent 成员变动时失效缓存，并在命中欢迎语配置时入队一条待投递。
func (n *Notify) handleSpaceMemberEvent(data []byte, commit config.EventCommit) {
	var req map[string]interface{}
	if err := util.ReadJsonByByte(data, &req); err != nil {
		n.Warn("解析SpaceMember事件失败", zap.Error(err))
		commit(nil)
		return
	}
	spaceID, _ := req["space_id"].(string)
	uid, _ := req["uid"].(string)
	if spaceID != "" {
		n.memberCache.invalidate(spaceID)
	}
	// 低延迟入队：命中欢迎语配置的首次加入 human 成员写一条 pending 行。入队失败
	// 只记日志、绝不阻塞或回滚已完成的加入（对账 goroutine 会兜底补齐）。
	if n.spaceWelcome != nil && spaceID != "" && uid != "" {
		n.spaceWelcome.handleMemberJoin(spaceID, uid)
	}
	commit(nil)
}

// Start 启动欢迎语的对账 + 发送 worker goroutine（模块生命周期钩子）。
func (n *Notify) Start() error {
	// 通知 Bot 的 provisioning 放在 Start()（而非 New()）：New() 现在于
	// register.GetModules 阶段被调用，早于 module.Setup 跑迁移；若在 New() 里就
	// 写库，可能对尚未建好的 schema 做写入。Start() 在迁移完成后才执行，安全。
	// 带 panic recovery；失败可由 deliverNotification / worker 的 lazy 重试自愈。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				n.Error("ensureNotifyBot panic", zap.Any("recover", r))
			}
		}()
		n.ensureNotifyBotReady()
		if n.botOK.Load() {
			n.Info("Notify bot ready")
		}
	}()
	if n.spaceWelcome != nil {
		n.spaceWelcome.Start()
	}
	return nil
}

// Stop 停止欢迎语后台 goroutine（context.Cancel + 等待清理退出）。
func (n *Notify) Stop() error {
	if n.spaceWelcome != nil {
		n.spaceWelcome.Stop()
	}
	return nil
}

// sendNotify handles POST /v1/internal/notify
func (n *Notify) sendNotify(c *wkhttp.Context) {
	var req NotifyReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseErrorWithStatus(errors.New("参数格式错误"), http.StatusBadRequest)
		return
	}
	// Payload dropped its binding:"required" so card requests (Payload absent,
	// Card / DocsCard present) bind cleanly. Payload / Card / DocsCard are
	// mutually exclusive (contract). Presence uses != nil for Payload (an
	// explicit `{}` counts as "caller intended to send a payload" and must not
	// silently combine with Card / DocsCard); the legacy "payload不能为空" 400
	// still fires when nothing meaningful is provided.
	present := 0
	if req.Payload != nil {
		present++
	}
	if req.Card != nil {
		present++
	}
	if req.DocsCard != nil {
		present++
	}
	if req.ApprovalCard != nil {
		present++
	}
	switch {
	case present > 1:
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardInvalid, nil, nil)
		return
	case req.Card == nil && req.DocsCard == nil && req.ApprovalCard == nil && len(req.Payload) == 0:
		c.ResponseErrorWithStatus(errors.New("payload不能为空"), http.StatusBadRequest)
		return
	}
	capability, _ := c.Get(notifyCapabilityContextKey)
	typedCapability, _ := capability.(notifyCapability)
	if !n.notifyCapabilityAllows(typedCapability, &req) {
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardNotAllowed, nil, nil)
		return
	}

	resp, err := n.dispatchNotify(&req, typedCapability)
	if err != nil {
		if errors.Is(err, errNotifyCardNotAllowed) {
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardNotAllowed, nil, nil)
			return
		}
		if errors.Is(err, errNotifyCardInvalid) {
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardInvalid, nil, nil)
			return
		}
		n.Error("投递通知失败", zap.Error(err), zap.String("space_id", req.SpaceID))
		c.ResponseErrorWithStatus(errors.New("internal error"), http.StatusInternalServerError)
		return
	}
	c.Response(resp)
}

// dispatchNotify routes a single request to the correct producer path (when a
// structured Card / DocsCard is present) or the legacy text path.
func (n *Notify) dispatchNotify(req *NotifyReq, capability notifyCapability) (*NotifyResp, error) {
	if req != nil && req.Card != nil {
		return n.deliverCardNotification(req)
	}
	if req != nil && req.DocsCard != nil {
		return n.deliverDocsCardNotification(req)
	}
	if req != nil && req.ApprovalCard != nil {
		return n.deliverApprovalCardNotification(req, capability.Action)
	}
	return n.deliverNotification(req)
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
	capabilityValue, _ := c.Get(notifyCapabilityContextKey)
	capability, _ := capabilityValue.(notifyCapability)
	if capability.Kind == notifyCapabilityDocs || capability.Kind == notifyCapabilityAction {
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardNotAllowed, nil, nil)
		return
	}
	// Preflight the whole batch before delivering any earlier text item. This
	// preserves the zero-transport guarantee when a later entry is a card.
	// Card / DocsCard notifications are single-endpoint only (they fan out through
	// the carddispatch producer), so any card entry in a batch is rejected outright.
	for i := range req.Notifications {
		if req.Notifications[i].Card != nil || req.Notifications[i].DocsCard != nil || req.Notifications[i].ApprovalCard != nil {
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardInvalid, nil, nil)
			return
		}
		if cardmsg.IsCardPayload(req.Notifications[i].Payload) {
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardNotAllowed, nil, nil)
			return
		}
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

func (n *Notify) notifyCapabilityAllows(capability notifyCapability, req *NotifyReq) bool {
	if n == nil || req == nil {
		return false
	}
	switch capability.Kind {
	case notifyCapabilityLegacy:
		return req.DocsCard == nil && req.ApprovalCard == nil
	case notifyCapabilityDocs:
		return req.DocsCard != nil && req.Card == nil && req.ApprovalCard == nil && req.Payload == nil
	case notifyCapabilityAction:
		return req.ApprovalCard != nil && req.Card == nil && req.DocsCard == nil && req.Payload == nil &&
			n.actionService != nil && n.actionService.CanNotify(capability.Action, req.ApprovalCard.ActionType)
	default:
		return false
	}
}

// ActionNotifyProducerID maps one route-bound owner to a stable internal
// producer without putting unbounded owner text into registry identifiers.
func ActionNotifyProducerID(owner string) carddispatch.ProducerID {
	digest := sha256.Sum256([]byte(owner))
	return carddispatch.ProducerID("action-notify-" + hex.EncodeToString(digest[:8]))
}

// deliverNotification 校验、过滤、投递
func (n *Notify) deliverNotification(req *NotifyReq) (*NotifyResp, error) {
	if req != nil && cardmsg.IsCardPayload(req.Payload) {
		return nil, errNotifyCardNotAllowed
	}
	if req == nil {
		return nil, errors.New("request不能为空")
	}
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
	n.ensureNotifyBotReady()
	if !n.botOK.Load() {
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
