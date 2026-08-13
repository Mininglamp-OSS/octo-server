package bot_mention

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"github.com/Mininglamp-OSS/octo-server/pkg/botevent"
	"go.uber.org/zap"
)

type botMentionRobotService interface {
	ExistRobot(uid string) (bool, error)
	// GetCreatorUID 查 Bot 的创建者(主人)uid。带缓存;Bot 不存在或无 creator_uid 时
	// 返回空串 + nil error(见 modules/robot/api.go 的契约)。
	GetCreatorUID(robotID string) (string, error)
	PrepareBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (robot.PreparedBotTypedEvent, error)
}

type botMentionClaimStore interface {
	Lookup(key, sha string) (claimOutcome, error)
	Begin(key, sha string) (claimOutcome, *claimLease, error)
	Commit(lease *claimLease, event robot.PreparedBotTypedEvent) (bool, error)
	Release(lease *claimLease) (bool, error)
}

type BotMention struct {
	robots         botMentionRobotService
	claims         botMentionClaimStore
	gate           featureGate
	internalToken  string
	metrics        botMentionMetricRecorder
	now            func() time.Time
	notifyBotEvent func(robotID string)
	log.Log
}

type mentionIngressResponse struct {
	Accepted bool   `json:"accepted"`
	Replay   bool   `json:"replay"`
	EventID  int64  `json:"event_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func New(ctx *config.Context) *BotMention {
	logger := log.NewTLog("BotMention")
	token, tokenErr := resolveBotMentionInternalToken(os.Getenv)
	if tokenErr != nil {
		logger.Error(tokenErr.Error())
	}
	return &BotMention{
		robots:        robot.NewService(ctx),
		claims:        newRedisClaimStore(ctx),
		gate:          featureGateFromEnv(),
		internalToken: token,
		metrics:       defaultBotMentionMetrics,
		now:           time.Now,
		notifyBotEvent: func(robotID string) {
			botevent.Notify(ctx.GetConfig(), robotID)
		},
		Log: logger,
	}
}

func (m *BotMention) Route(r *wkhttp.WKHttp) {
	internal := r.Group("/v1/internal", m.internalAuthMiddleware())
	internal.POST("/bot-mentions", m.create)
	internal.POST("/bot-owner", m.botOwner)
}

// botOwner 回答「某个 Bot 的主人(创建者)是谁」。
//
// 为什么需要这个:HTML 文档的权限判定要的是 **Bot 的主人**,不是 Bot 自己 —— octo-doc 的
// creator_uid 按设计存的永远是人的 uid(Bot 发布时存它主人的 uid,"bot and owner share
// author")。octo-doc 判权限时只拿到一个 bot_uid,而它换 OwnerUID 的唯一途径 VerifyBot 需要
// Bot 的 **token**,服务间调用没有。归属数据只在这边(robot.creator_uid),所以由这边回答。
//
// 只回一个 uid,不回 Bot 的其它信息:调用方需要的就是这一个字段,回得多只会让它长出别的依赖。
// Bot 不存在 / 没有 creator_uid ⇒ owner_uid 为空串 + 200。空串对调用方就是「判不出」,
// 而它对判不出的处理是拒绝(fail closed)—— 用 404 反而会被误当成「服务不可用」而重试。
func (m *BotMention) botOwner(c *wkhttp.Context) {
	var body struct {
		BotUID string `json:"bot_uid"`
	}
	// 错误响应一律走 respondBotMention* 那几个 helper(内部是 httperr.ResponseErrorLWithStatus)。
	// 本模块有条 lint 测试禁止裸 c.ResponseError —— i18n 要求错误码可翻译。
	if err := c.BindJSON(&body); err != nil {
		respondBotMentionInvalid(c, "body")
		return
	}
	botUID := strings.TrimSpace(body.BotUID)
	if botUID == "" {
		respondBotMentionInvalid(c, "bot_uid")
		return
	}
	ownerUID, err := m.robots.GetCreatorUID(botUID)
	if err != nil {
		m.Error("bot owner lookup failed", zap.String("bot_uid", botUID), zap.Error(err))
		respondBotMentionStoreFailed(c)
		return
	}
	c.Response(map[string]string{"owner_uid": strings.TrimSpace(ownerUID)})
}

func (m *BotMention) internalAuthMiddleware() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		started := time.Now()
		token := c.GetHeader(internalTokenHeader)
		if m.internalToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(m.internalToken)) != 1 {
			respondBotMentionUnauthorized(c)
			c.Abort()
			if m.metrics != nil {
				m.metrics.ObserveIngress("unauthorized", time.Since(started))
			}
			return
		}
		c.Next()
	}
}

func (m *BotMention) create(c *wkhttp.Context) {
	started := time.Now()
	result := "error"
	defer func() {
		if m.metrics != nil {
			m.metrics.ObserveIngress(result, time.Since(started))
		}
	}()

	request, err := decodeMentionRequest(c)
	if err != nil {
		result = "invalid"
		respondBotMentionInvalid(c, "body")
		return
	}
	mention, err := normalizeMentionRequest(request)
	if err != nil {
		result = "invalid"
		respondBotMentionInvalid(c, invalidField(err))
		return
	}

	claimKey := mentionClaimKey(mention.BotUID, mention.IdempotencyKey)
	fingerprint := mentionFingerprint(mention)
	existing, err := m.claims.Lookup(claimKey, fingerprint)
	if err != nil {
		m.Error("bot mention idempotency lookup failed", zap.Error(err), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		respondBotMentionStoreFailed(c)
		return
	}
	if handled, claimResult := respondBotMentionClaimOutcome(c, existing); handled {
		result = claimResult
		if claimResult == "replay" {
			m.logOutcome(mention, claimResult, existing.EventID, claimKey)
		}
		return
	}

	if !m.gate.Allows(mention.DocID, mention.SpaceID) {
		result = "disabled"
		m.logOutcome(mention, result, 0, claimKey)
		c.Response(mentionIngressResponse{Accepted: false, Replay: false, Reason: "disabled"})
		return
	}

	exists, err := m.robots.ExistRobot(mention.BotUID)
	if err != nil {
		m.Error("bot mention user bot lookup failed", zap.Error(err), zap.String("bot_uid", mention.BotUID))
		respondBotMentionStoreFailed(c)
		return
	}
	if !exists {
		result = "not_found"
		respondBotMentionNotFound(c)
		return
	}

	outcome, lease, err := m.claims.Begin(claimKey, fingerprint)
	if err != nil {
		m.Error("bot mention idempotency claim failed", zap.Error(err), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		respondBotMentionStoreFailed(c)
		return
	}
	if handled, claimResult := respondBotMentionClaimOutcome(c, outcome); handled {
		result = claimResult
		if claimResult == "replay" {
			m.logOutcome(mention, claimResult, outcome.EventID, claimKey)
		}
		return
	}
	if outcome.State != claimAcquired || lease == nil {
		m.Error("bot mention idempotency claim returned invalid state", zap.Int("state", int(outcome.State)), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		respondBotMentionStoreFailed(c)
		return
	}
	eventData := mentionEventData(mention, m.now().Unix())
	enqueueStarted := time.Now()
	prepared, err := m.robots.PrepareBotTypedEvent(mention.BotUID, docCommentMentionEventType, eventData)
	if err != nil {
		if m.metrics != nil {
			m.metrics.ObserveEnqueue("error", time.Since(enqueueStarted))
		}
		if released, releaseErr := m.claims.Release(lease); releaseErr != nil || !released {
			m.Warn("bot mention claim release did not apply",
				zap.Bool("released", released), zap.Error(releaseErr), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		}
		m.Error("bot mention event preparation failed", zap.Error(err), zap.String("bot_uid", mention.BotUID), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		respondBotMentionStoreFailed(c)
		return
	}

	committed, commitErr := m.claims.Commit(lease, prepared)
	if commitErr != nil {
		current, lookupErr := m.claims.Lookup(claimKey, fingerprint)
		if lookupErr == nil && current.State == claimReplay {
			m.notifyQueue(mention.BotUID)
			if m.metrics != nil {
				m.metrics.ObserveEnqueue("accepted", time.Since(enqueueStarted))
			}
			result = "replay"
			m.logOutcome(mention, result, current.EventID, claimKey)
			c.Response(mentionIngressResponse{Accepted: true, Replay: true, EventID: current.EventID})
			return
		}
		if m.metrics != nil {
			m.metrics.ObserveEnqueue("error", time.Since(enqueueStarted))
		}
		m.Error("bot mention atomic enqueue failed",
			zap.NamedError("commit_error", commitErr), zap.NamedError("lookup_error", lookupErr),
			zap.Int64("event_id", prepared.EventID), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
		respondBotMentionStoreFailed(c)
		return
	}
	if !committed {
		current, lookupErr := m.claims.Lookup(claimKey, fingerprint)
		if lookupErr != nil {
			m.Error("bot mention stale enqueue outcome lookup failed", zap.Error(lookupErr), zap.String("idempotency_hash", mentionClaimLogHash(claimKey)))
			respondBotMentionStoreFailed(c)
			return
		}
		if handled, claimResult := respondBotMentionClaimOutcome(c, current); handled {
			result = claimResult
			if claimResult == "replay" {
				m.notifyQueue(mention.BotUID)
				m.logOutcome(mention, claimResult, current.EventID, claimKey)
			}
			return
		}
		result = "conflict"
		respondBotMentionInProgress(c)
		return
	}
	if m.metrics != nil {
		m.metrics.ObserveEnqueue("accepted", time.Since(enqueueStarted))
	}
	m.notifyQueue(mention.BotUID)

	result = "accepted"
	m.logOutcome(mention, result, prepared.EventID, claimKey)
	c.Response(mentionIngressResponse{Accepted: true, Replay: false, EventID: prepared.EventID})
}

func decodeMentionRequest(c *wkhttp.Context) (mentionRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	var request mentionRequest
	if err := decoder.Decode(&request); err != nil {
		return mentionRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return mentionRequest{}, errors.New("bot mention request contains multiple JSON values")
		}
		return mentionRequest{}, err
	}
	return request, nil
}

func (m *BotMention) logOutcome(mention normalizedMention, result string, eventID int64, claimKey string) {
	m.Info("bot mention ingress completed",
		zap.String("result", result),
		zap.Int64("event_id", eventID),
		zap.String("bot_uid", mention.BotUID),
		zap.String("idempotency_hash", mentionClaimLogHash(claimKey)),
	)
}

func (m *BotMention) notifyQueue(robotID string) {
	if m.notifyBotEvent != nil {
		m.notifyBotEvent(robotID)
	}
}

func respondBotMentionClaimOutcome(c *wkhttp.Context, outcome claimOutcome) (bool, string) {
	switch outcome.State {
	case claimMissing, claimAcquired:
		return false, ""
	case claimPending:
		respondBotMentionInProgress(c)
		return true, "conflict"
	case claimReplay:
		c.Response(mentionIngressResponse{Accepted: true, Replay: true, EventID: outcome.EventID})
		return true, "replay"
	case claimConflict:
		respondBotMentionConflict(c)
		return true, "conflict"
	default:
		respondBotMentionStoreFailed(c)
		return true, "error"
	}
}

func mentionEventData(mention normalizedMention, enqueuedAt int64) map[string]interface{} {
	data := map[string]interface{}{
		"idempotency_key": mention.IdempotencyKey,
		"doc_id":          mention.DocID,
		"comment_id":      mention.CommentID,
		"thread_id":       mention.ThreadID,
		"from_uid":        mention.FromUID,
		"bot_uid":         mention.BotUID,
		"text":            mention.Text,
		"enqueued_at":     enqueuedAt,
	}
	if mention.ParentID != "" {
		data["parent_id"] = mention.ParentID
	}
	// 只在非默认类型时出现:普通文档的事件载荷保持和加这个字段之前逐字节一致,
	// 免得既有消费者的字段集断言(插件那份签进仓库的快照)无端变红。
	if mention.DocKind != "" {
		data["doc_kind"] = mention.DocKind
	}
	if mention.URL != "" {
		data["url"] = mention.URL
	}
	if mention.SpaceID != "" {
		data["space_id"] = mention.SpaceID
	}
	return data
}
