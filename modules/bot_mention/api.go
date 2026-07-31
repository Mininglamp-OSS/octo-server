package bot_mention

import (
	"crypto/subtle"
	"net/http"
	"os"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/robot"
	"go.uber.org/zap"
)

type botMentionRobotService interface {
	ExistRobot(uid string) (bool, error)
	EnqueueBotTypedEvent(robotID, eventType string, eventData map[string]interface{}) (int64, error)
}

type botMentionClaimStore interface {
	Lookup(key, sha string) (claimOutcome, error)
	Begin(key, sha string) (claimOutcome, *claimLease, error)
	Confirm(lease *claimLease, eventID int64) (bool, error)
	Release(lease *claimLease) (bool, error)
}

type BotMention struct {
	robots        botMentionRobotService
	claims        botMentionClaimStore
	gate          featureGate
	internalToken string
	metrics       botMentionMetricRecorder
	now           func() time.Time
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
	token := os.Getenv(internalTokenEnv)
	if token == "" {
		logger.Error("OCTO_DOCS_BOT_MENTION_TOKEN not set; internal bot mention API will reject all requests")
	}
	return &BotMention{
		robots:        robot.NewService(ctx),
		claims:        newRedisClaimStore(ctx),
		gate:          featureGateFromEnv(),
		internalToken: token,
		metrics:       defaultBotMentionMetrics,
		now:           time.Now,
		Log:           logger,
	}
}

func (m *BotMention) Route(r *wkhttp.WKHttp) {
	internal := r.Group("/v1/internal", m.internalAuthMiddleware())
	internal.POST("/bot-mentions", m.create)
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

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
	var request mentionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
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
		m.Error("bot mention idempotency lookup failed", zap.Error(err), zap.String("claim_key", claimKey))
		respondBotMentionStoreFailed(c)
		return
	}
	if handled, claimResult := respondBotMentionClaimOutcome(c, existing); handled {
		result = claimResult
		return
	}

	if !m.gate.Allows(mention.DocID, mention.SpaceID) {
		result = "disabled"
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
		result = "invalid"
		respondBotMentionNotFound(c)
		return
	}

	outcome, lease, err := m.claims.Begin(claimKey, fingerprint)
	if err != nil {
		m.Error("bot mention idempotency claim failed", zap.Error(err), zap.String("claim_key", claimKey))
		respondBotMentionStoreFailed(c)
		return
	}
	if handled, claimResult := respondBotMentionClaimOutcome(c, outcome); handled {
		result = claimResult
		return
	}
	if outcome.State != claimAcquired || lease == nil {
		m.Error("bot mention idempotency claim returned invalid state", zap.Int("state", int(outcome.State)), zap.String("claim_key", claimKey))
		respondBotMentionStoreFailed(c)
		return
	}

	eventData := mentionEventData(mention, m.now().Unix())
	enqueueStarted := time.Now()
	eventID, err := m.robots.EnqueueBotTypedEvent(mention.BotUID, docCommentMentionEventType, eventData)
	if err != nil {
		if m.metrics != nil {
			m.metrics.ObserveEnqueue("error", time.Since(enqueueStarted))
		}
		if released, releaseErr := m.claims.Release(lease); releaseErr != nil || !released {
			m.Warn("bot mention claim release did not apply",
				zap.Bool("released", released), zap.Error(releaseErr), zap.String("claim_key", claimKey))
		}
		m.Error("bot mention event enqueue failed", zap.Error(err), zap.String("bot_uid", mention.BotUID), zap.String("claim_key", claimKey))
		respondBotMentionStoreFailed(c)
		return
	}
	if m.metrics != nil {
		m.metrics.ObserveEnqueue("accepted", time.Since(enqueueStarted))
	}

	if confirmed, confirmErr := m.claims.Confirm(lease, eventID); confirmErr != nil || !confirmed {
		// The event is already in the bot queue. Do not report a false failure or
		// attempt to roll it back; the consumer deduplicates on idempotency_key.
		m.Warn("bot mention idempotency confirm did not apply",
			zap.Bool("confirmed", confirmed), zap.Error(confirmErr),
			zap.Int64("event_id", eventID), zap.String("claim_key", claimKey))
	}

	result = "accepted"
	c.Response(mentionIngressResponse{Accepted: true, Replay: false, EventID: eventID})
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
	if mention.URL != "" {
		data["url"] = mention.URL
	}
	if mention.SpaceID != "" {
		data["space_id"] = mention.SpaceID
	}
	return data
}
